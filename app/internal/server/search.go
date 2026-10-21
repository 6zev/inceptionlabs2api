package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/upstream"
)

// Search request bounds are the upstream's published ranges. Out-of-range
// numbers are clamped, which is a translation; they are never a 400.
const (
	maxFanout   = 5
	maxDepth    = 3
	maxKeepTopK = 50
)

// webSearchRequest is the POST /v1/web_search body. Config is optional, so
// it stays a pointer to distinguish "absent" from all-zero.
type webSearchRequest struct {
	Query  string             `json:"query"`
	Config *webSearchConfigIn `json:"config"`
}

type webSearchConfigIn struct {
	Fanout   int    `json:"fanout"`
	Depth    int    `json:"depth"`
	KeepTopK int    `json:"keep_top_k"`
	Effort   string `json:"effort"`
}

// webSearchResponse mirrors the simple-chat contract exactly:
// {"object":"web_search","queries":[...],"results":[{url,title,snippet,cite_index}]}.
type webSearchResponse struct {
	Object  string            `json:"object"`
	Queries []string          `json:"queries"`
	Results []webSearchResult `json:"results"`
}

type webSearchResult struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Snippet   string `json:"snippet"`
	CiteIndex int    `json:"cite_index"`
}

// webSearch bridges the upstream retrieval pipeline into a stateless search
// response. It never streams: the whole NDJSON run is consumed under one
// request, and the endpoint answers only once sources exist.
func (s *Server) webSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		openai.WriteError(w, openai.New(http.StatusMethodNotAllowed, "invalid_request_error", openai.CodeMethodNotAllowed, "",
			"method not allowed; use POST"))
		return
	}
	s.requests.Add(1)

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.fail(w, openai.New(http.StatusRequestEntityTooLarge, "invalid_request_error", openai.CodeRequestTooLarge, "",
				fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes)))
			return
		}
		s.fail(w, openai.Invalid(fmt.Sprintf("read request body: %v", err)))
		return
	}
	req, re := decodeWebSearch(body)
	if re != nil {
		s.fail(w, re)
		return
	}
	if s.se == nil {
		s.fail(w, openai.New(http.StatusServiceUnavailable, "server_error", openai.CodeUpstreamUnavailable, "",
			"web search backend is not configured"))
		return
	}
	if s.searchDisabled() {
		s.fail(w, openai.New(http.StatusServiceUnavailable, "server_error", openai.CodeSearchUnavailable, "",
			"web retrieval is not configured on this deployment"))
		return
	}

	res, err := s.se.Search(r.Context(), searchRequest(req), s.cfg.SearchTimeout)
	if err != nil {
		// A client that walked away has no socket to answer on; writing an
		// envelope would only log a spurious upstream error.
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return
		}
		s.fail(w, s.searchError(err))
		return
	}
	// Results derive from the caller's query; keep them out of shared caches.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, webSearchResponse{
		Object:  "web_search",
		Queries: nonNil(res.Queries),
		Results: results(res.Sources),
	})
}

// searchRequest maps the validated client body onto the upstream wire shape.
func searchRequest(req *webSearchRequest) *upstream.SearchRequest {
	in := req.Config
	if in == nil {
		in = &webSearchConfigIn{}
	}
	effort := in.Effort
	if effort == "" {
		effort = "instant"
	}
	// effort is the upstream's enum: pass it through unchanged.
	return &upstream.SearchRequest{
		Query: req.Query,
		Config: upstream.SearchConfig{
			Fanout:   clamp(in.Fanout, 1, maxFanout, 3),
			Depth:    clamp(in.Depth, 1, maxDepth, 1),
			KeepTopK: clamp(in.KeepTopK, 1, maxKeepTopK, 10),
			Effort:   effort,
		},
		Timezone: "UTC",
		Language: "en-US",
	}
}

// clamp returns v bounded to [lo,hi], or def when v is unset (zero).
func clamp(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// results converts ranked sources into the response list. Snippets are always
// empty: the upstream publishes none, and the gateway does not invent one.
func results(sources []upstream.SearchSource) []webSearchResult {
	out := make([]webSearchResult, 0, len(sources))
	for i, src := range sources {
		out = append(out, webSearchResult{
			URL:       src.URL,
			Title:     src.Title,
			Snippet:   "",
			CiteIndex: i + 1,
		})
	}
	return out
}

func nonNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// decodeWebSearch rejects only what the upstream call cannot express:
// malformed JSON, a body that is not an object, a missing/non-string/empty
// query, and a config that is not an object. Unknown keys are dropped and
// numeric bounds are clamped; effort is passed through (SPEC §1.1).
func decodeWebSearch(body []byte) (*webSearchRequest, *openai.RequestError) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&raw); err != nil {
		return nil, openai.Invalid(fmt.Sprintf("invalid JSON body: %v", err))
	}
	// Anything other than EOF after the object — valid JSON or garbage — is
	// rejected: a prefix that a front proxy or WAF might parse differently
	// must not be silently tolerated.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, openai.Invalid("invalid JSON body: unexpected data after the request object")
	}
	if raw == nil {
		return nil, openai.Invalid("request body must be a JSON object")
	}

	var req webSearchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, openai.Invalid(fmt.Sprintf("invalid request: %v", err))
	}
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		return nil, &openai.RequestError{Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: openai.CodeInvalidRequest, Param: "query", Message: `field "query" must be a non-empty string`}
	}
	if req.Config != nil {
		var cfgRaw map[string]json.RawMessage
		if err := json.Unmarshal(raw["config"], &cfgRaw); err != nil || cfgRaw == nil {
			// null is treated as an absent config.
			if string(bytes.TrimSpace(raw["config"])) == "null" {
				req.Config = nil
			} else {
				return nil, &openai.RequestError{Status: http.StatusBadRequest, Type: "invalid_request_error",
					Code: openai.CodeInvalidRequest, Param: "config", Message: "config must be an object"}
			}
		}
	}
	return &req, nil
}

// searchError maps search failures onto the chat path's status/code surface.
// It increments upstream_errors itself only for the failures it classifies;
// the fallback delegates to upstreamError, which owns that counter, so a
// single failure is never counted twice.
func (s *Server) searchError(err error) *openai.RequestError {
	var se *upstream.SearchError
	if errors.As(err, &se) {
		s.upErrors.Add(1)
		s.logf("upstream: search failed: %s", truncate(se.Message, 300))
		msg := "upstream search pipeline failed"
		if m := strings.TrimSpace(se.Message); m != "" {
			msg = truncate(m, 1000)
		}
		return openai.New(http.StatusBadGateway, "upstream_error", openai.CodeUpstreamError, "", msg)
	}
	switch {
	case errors.Is(err, upstream.ErrSearchIncomplete):
		s.upErrors.Add(1)
		return openai.New(http.StatusBadGateway, "upstream_error", openai.CodeUpstreamStream, "",
			"upstream closed the search stream before returning sources")
	case errors.Is(err, context.Canceled):
		return openai.New(http.StatusRequestTimeout, "invalid_request_error", openai.CodeInvalidRequest, "",
			"request canceled by the client")
	}
	return s.upstreamError(err)
}
