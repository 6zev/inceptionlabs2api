package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/tools"
	"inceptionlabs2api/internal/upstream"
)

// nativeSearchBudget caps how many retrieval runs one client request may
// trigger. Past it, the next call is answered with an error result so the
// model can still wrap up; the stream is never aborted.
const nativeSearchBudget = 3

// nativeSearchTool returns the gateway-served web_search definition, or nil
// when the feature is off or no retrieval backend is attached. A nil tool
// makes planTools take exactly the pre-native path.
func (s *Server) nativeSearchTool() *tools.Tool {
	if !s.cfg.NativeWebSearch || s.se == nil {
		return nil
	}
	t := tools.NativeSearchTool()
	return &t
}

// nativeSearchContent serialises one retrieval run as the tool result the
// model sees. It is the same envelope /v1/web_search returns, plus as_of so
// the model can reason about recency. answer carries the upstream's
// content-grounded synthesis when the run reached one; its [n] citation
// markers refer to the results array (cite_index, 1-based, in this order —
// never renumbered or reordered). The field is omitted when upstream
// produced no answer, and the model works from titles/urls as before.
func nativeSearchContent(res *upstream.SearchResult, asOf string) string {
	body, err := json.Marshal(struct {
		Object  string            `json:"object"`
		AsOf    string            `json:"as_of"`
		Queries []string          `json:"queries"`
		Results []webSearchResult `json:"results"`
		Answer  string            `json:"answer,omitempty"`
	}{
		Object:  "web_search",
		AsOf:    asOf,
		Queries: nonNil(res.Queries),
		Results: results(res.Sources),
		Answer:  res.Answer,
	})
	if err != nil {
		return `{"error":"search result could not be encoded"}`
	}
	return string(body)
}

// errorContent renders a backend or argument failure as a tool result.
func errorContent(msg string) string {
	body, err := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: msg})
	if err != nil {
		return `{"error":"search failed"}`
	}
	return string(body)
}

// nativeSearch runs one call against the attached retrieval backend under a
// bounded timeout. The returned content is always a tool result; err is set
// only for a hard local failure that must surface as an OpenAI error frame.
func (s *Server) nativeSearch(ctx context.Context, call tools.Call) (string, error) {
	if s.se == nil {
		return "", upstream.ErrUnavailable
	}
	req, re := decodeWebSearch([]byte(call.Arguments))
	if re != nil {
		return errorContent(re.Message), nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.SearchTimeout)
	defer cancel()
	res, err := s.se.Search(ctx, searchRequest(req), s.cfg.SearchTimeout)
	if err != nil {
		var se *upstream.SearchError
		switch {
		case errors.As(err, &se):
			s.logf("tools: native search failed: %s", truncate(se.Message, 300))
			return errorContent(truncate(se.Message, 1000)), nil
		case errors.Is(err, upstream.ErrSearchIncomplete):
			return errorContent("upstream search closed before returning results"), nil
		case errors.Is(err, upstream.ErrIdleTimeout), errors.Is(err, context.DeadlineExceeded):
			s.logf("tools: native search timed out: %v", err)
			return errorContent("search timed out"), nil
		}
		return "", err
	}
	return nativeSearchContent(res, s.now().UTC().Format("2006-01-02")), nil
}

// nativeRound splits a parsed round into the calls the gateway serves itself
// and the calls that belong to the client.
func nativeRound(calls []tools.Call) (native, client []tools.Call) {
	for _, c := range calls {
		if c.Name == tools.NativeSearchName {
			native = append(native, c)
		} else {
			client = append(client, c)
		}
	}
	return native, client
}

// serveNativeCalls answers every web_search call in one round and returns the
// history a re-ask must carry: the assistant turn as emitted plus one result
// per call. budget is the number of searches already run.
func (s *Server) serveNativeCalls(ctx context.Context, calls []tools.Call, content string, budget int) ([]openai.History, int, error) {
	assistant := openai.History{Role: "assistant"}
	if content != "" {
		assistant.Texts = []string{content}
	}
	out := make([]openai.History, 0, len(calls)+1)
	for _, c := range calls {
		id := tools.NewID()
		assistant.Calls = append(assistant.Calls, openai.ToolCall{
			ID:       id,
			Type:     "function",
			Function: openai.FunctionCall{Name: c.Name, Arguments: c.Arguments},
		})
		result := `{"error":"search budget exhausted"}`
		if budget < nativeSearchBudget {
			r, err := s.nativeSearch(ctx, c)
			if err != nil {
				return nil, budget, err
			}
			result = r
			budget++
		}
		out = append(out, openai.History{Role: "user", Results: []openai.ToolResult{{CallID: id, Content: result}}})
	}
	return append([]openai.History{assistant}, out...), budget, nil
}

// nativeAbort reports whether a client cancellation means nothing should be
// written. It mirrors the check the plain streaming path makes inline.
func nativeAbort(r *http.Request, err error) bool {
	return errors.Is(err, context.Canceled) && r.Context().Err() != nil
}
