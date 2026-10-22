package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/upstream"
)

// Search request bounds are the ranges the upstream publishes in
// /api/search-lab/config (`limits`). Out-of-range numbers are clamped, which
// is a translation; they are never a 400. keep_top_k has no published range
// and is therefore not bounded here (SPEC §7.1).
const (
	maxFanout = 5
	maxDepth  = 3
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
// answer is the upstream's content-grounded synthesized text (citation
// markers align with the results order); it is omitted when the run
// produced none, so existing clients see one new optional field only.
type webSearchResponse struct {
	Object  string            `json:"object"`
	Queries []string          `json:"queries"`
	Results []webSearchResult `json:"results"`
	Answer  string            `json:"answer,omitempty"`
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
		methodNotAllowed(w, http.MethodPost)
		return
	}
	s.requests.Add(1)

	body, re := s.readJSONBody(w, r)
	if re != nil {
		s.fail(w, re)
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
	res, err := s.se.Search(r.Context(), searchRequest(req), s.cfg.SearchTimeout)
	if err != nil {
		// A client that walked away has no socket to answer on; writing an
		// envelope would only log a spurious upstream error.
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return
		}
		s.fail(w, s.upstreamError(err))
		return
	}
	// Results derive from the caller's query; keep them out of shared caches.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, webSearchResponse{
		Object:  "web_search",
		Queries: nonNil(res.Queries),
		Results: results(res.Sources),
		Answer:  res.Answer,
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
			Fanout:   upstream.ClampInt(in.Fanout, 1, maxFanout, 3),
			Depth:    upstream.ClampInt(in.Depth, 1, maxDepth, 1),
			KeepTopK: keepTopK(in.KeepTopK),
			Effort:   effort,
		},
		Timezone: "UTC",
		Language: "en-US",
	}
}

// keepTopK defaults an unset value and passes everything else through: the
// upstream publishes no range for keepTopK, so a gateway bound would be an
// invented policy (§1.1, §7.1).
func keepTopK(v int) int {
	if v == 0 {
		return 10
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
	raw, re := decodeJSONObject(body)
	if re != nil {
		return nil, re
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
