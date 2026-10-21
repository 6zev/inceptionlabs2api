package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/upstream"
)

// Search request bounds. The query cap is far above any real question and
// keeps one request from fanning out into an unbounded plan prompt.
const (
	maxSearchQuery = 2000
	maxFanout      = 5
	maxDepth       = 3
	maxKeepTopK    = 50
)

// searchEfforts are the accepted effort values. Only "instant" is exercised
// by the first-party playground; fast/deep are documented pass-throughs.
var searchEfforts = map[string]bool{"instant": true, "fast": true, "deep": true}

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

var searchTopLevelKeys = []string{"query", "config"}
var searchConfigKeys = []string{"fanout", "depth", "keep_top_k", "effort"}

// webSearch bridges the upstream retrieval pipeline into a stateless search
// response. It never streams: the whole NDJSON run is consumed under one
// request, and the endpoint answers only once sources exist.
func (s *Server) webSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
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

// decodeWebSearch validates the body with the same no-silent-drops policy as
// the chat path: unknown keys, bad types, an empty query, and out-of-enum
// values are all 400s that name the offending field.
func decodeWebSearch(body []byte) (*webSearchRequest, *openai.RequestError) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&raw); err != nil {
		return nil, openai.Invalid(fmt.Sprintf("invalid JSON body: %v", err))
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err == nil {
		return nil, openai.Invalid("invalid JSON body: unexpected data after the request object")
	}
	if raw == nil {
		return nil, openai.Invalid("request body must be a JSON object")
	}
	var unknown []string
	for key := range raw {
		if !contains(searchTopLevelKeys, key) {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, unsupported(unknown[0], unknown, searchTopLevelKeys)
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
	if len([]rune(req.Query)) > maxSearchQuery {
		return nil, &openai.RequestError{Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: openai.CodeInvalidRequest, Param: "query",
			Message: fmt.Sprintf("query exceeds %d characters", maxSearchQuery)}
	}

	if req.Config != nil {
		// Re-decode the config object alone so unknown keys can be named.
		var cfgRaw map[string]json.RawMessage
		if err := json.Unmarshal(raw["config"], &cfgRaw); err != nil {
			return nil, openai.Invalid("config must be an object")
		}
		var cfgUnknown []string
		for key := range cfgRaw {
			if !contains(searchConfigKeys, key) {
				cfgUnknown = append(cfgUnknown, key)
			}
		}
		if len(cfgUnknown) > 0 {
			sort.Strings(cfgUnknown)
			return nil, unsupported(cfgUnknown[0], cfgUnknown, searchConfigKeys)
		}
		if req.Config.Effort == "" {
			req.Config.Effort = "instant"
		}
		if !searchEfforts[req.Config.Effort] {
			return nil, &openai.RequestError{Status: http.StatusBadRequest, Type: "invalid_request_error",
				Code: openai.CodeInvalidRequest, Param: "config.effort",
				Message: fmt.Sprintf("config.effort must be one of instant, fast, deep; got %q", req.Config.Effort)}
		}
	}
	return &req, nil
}

func unsupported(param string, offenders, allowed []string) *openai.RequestError {
	quoted := make([]string, len(offenders))
	for i, o := range offenders {
		quoted[i] = fmt.Sprintf("%q", o)
	}
	allowedQ := make([]string, len(allowed))
	for i, a := range allowed {
		allowedQ[i] = fmt.Sprintf("%q", a)
	}
	return &openai.RequestError{Status: http.StatusBadRequest, Type: "invalid_request_error",
		Code: openai.CodeUnsupportedParam, Param: param,
		Message: fmt.Sprintf("unsupported parameter(s): %s; this endpoint accepts only: %s",
			strings.Join(quoted, ", "), strings.Join(allowedQ, ", "))}
}

func contains(list []string, key string) bool {
	for _, k := range list {
		if k == key {
			return true
		}
	}
	return false
}

// searchError maps search failures onto the chat path's status/code surface.
func (s *Server) searchError(err error) *openai.RequestError {
	s.upErrors.Add(1)
	var se *upstream.SearchError
	if errors.As(err, &se) {
		s.logf("upstream: search failed: %s", truncate(se.Message, 300))
		return openai.New(http.StatusBadGateway, "upstream_error", openai.CodeUpstreamError, "",
			"upstream search pipeline failed")
	}
	switch {
	case errors.Is(err, upstream.ErrSearchIncomplete):
		return openai.New(http.StatusBadGateway, "upstream_error", openai.CodeUpstreamStream, "",
			"upstream closed the search stream before returning sources")
	case errors.Is(err, context.Canceled):
		return openai.New(http.StatusRequestTimeout, "invalid_request_error", openai.CodeInvalidRequest, "",
			"request canceled by the client")
	}
	return s.upstreamError(err)
}
