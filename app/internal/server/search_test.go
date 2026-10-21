package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/pool"
	"inceptionlabs2api/internal/upstream"
)

func newSearchServer(t *testing.T, se Searcher) *Server {
	t.Helper()
	s := New(Config{
		Model:       "mercury-3",
		IdleTimeout: time.Second,
		Logf:        func(string, ...any) {},
	}, nil, fakePool{stats: pool.Stats{Size: 2, Min: 2, Max: 8}})
	s.SetSearcher(se)
	return s
}

// postSearch posts a body to /v1/web_search and returns the recorder.
func postSearch(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/web_search", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeSearch(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

// TestWebSearchResponseShape pins the Sliverkiss/simple-chat contract:
// {"object":"web_search","queries":[...],"results":[{url,title,snippet,cite_index}]}.
func TestWebSearchResponseShape(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{fn: func(int) (*upstream.SearchResult, error) {
		return &upstream.SearchResult{
			Queries: []string{"a", "b"},
			Sources: []upstream.SearchSource{
				{Title: "A", URL: "https://a.example", Score: 0.9},
				{Title: "B", URL: "https://b.example", Score: 0.5},
			},
		}, nil
	}}
	rec := postSearch(t, newSearchServer(t, se), `{"query":"latest tech news"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body)
	}
	var body struct {
		Object  string   `json:"object"`
		Queries []string `json:"queries"`
		Results []struct {
			URL       string `json:"url"`
			Title     string `json:"title"`
			Snippet   string `json:"snippet"`
			CiteIndex int    `json:"cite_index"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Object != "web_search" {
		t.Errorf("object = %q", body.Object)
	}
	if len(body.Queries) != 2 || body.Queries[0] != "a" {
		t.Errorf("queries = %+v", body.Queries)
	}
	if len(body.Results) != 2 {
		t.Fatalf("results = %d", len(body.Results))
	}
	if body.Results[0].URL != "https://a.example" || body.Results[0].Title != "A" ||
		body.Results[0].Snippet != "" || body.Results[0].CiteIndex != 1 {
		t.Errorf("results[0] = %+v", body.Results[0])
	}
	if body.Results[1].CiteIndex != 2 {
		t.Errorf("cite_index must be rank order, got %d", body.Results[1].CiteIndex)
	}
}

func TestWebSearchDefaultsAndClamping(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{}
	s := newSearchServer(t, se)
	if rec := postSearch(t, s, `{"query":"q"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := se.lastRequest()
	if got.Config.Fanout != 3 || got.Config.Depth != 1 || got.Config.KeepTopK != 10 || got.Config.Effort != "instant" {
		t.Fatalf("defaults = %+v", got.Config)
	}
	if got.Timezone != "UTC" || got.Language != "en-US" {
		t.Fatalf("timezone/language = %q/%q", got.Timezone, got.Language)
	}

	postSearch(t, s, `{"query":"q","config":{"fanout":9,"depth":0,"keep_top_k":500}}`)
	got = se.lastRequest()
	if got.Config.Fanout != 5 || got.Config.Depth != 1 || got.Config.KeepTopK != 50 {
		t.Fatalf("clamped = %+v", got.Config)
	}
}

func TestWebSearchRejectsBadRequests(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		code string
	}{
		{"empty object", `{}`, openai.CodeInvalidRequest},
		{"empty query", `{"query":""}`, openai.CodeInvalidRequest},
		{"whitespace query", `{"query":"   "}`, openai.CodeInvalidRequest},
		{"non-string query", `{"query":123}`, openai.CodeInvalidRequest},
		{"not json", `not json`, openai.CodeInvalidRequest},
		{"config not object", `{"query":"q","config":"x"}`, openai.CodeInvalidRequest},
		{"config array", `{"query":"q","config":[1]}`, openai.CodeInvalidRequest},
		{"trailing data", `{"query":"q"}{"x":1}`, openai.CodeInvalidRequest},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			se := &fakeSearcher{}
			rec := postSearch(t, newSearchServer(t, se), tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			var env openai.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("error envelope: %v", err)
			}
			if env.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q", env.Error.Code, tc.code)
			}
			if se.count() != 0 {
				t.Fatalf("a malformed request reached upstream %d times", se.count())
			}
		})
	}
}

// Pass-through policy: an unknown key, an unknown effort, and a very long
// query are all forwarded rather than rejected. Unknown keys are dropped;
// effort is the upstream's enum to judge.
func TestWebSearchPassesThroughUndeclaredInput(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{}
	long := strings.Repeat("x", 5000)
	body := `{"query":"` + long + `","top_k":3,"config":{"effort":"turbo","nope":1}}`
	rec := postSearch(t, newSearchServer(t, se), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	got := se.lastRequest()
	if got.Query != long {
		t.Fatalf("query forwarded = %d chars, want %d", len(got.Query), len(long))
	}
	if got.Config.Effort != "turbo" {
		t.Fatalf("effort = %q, want pass-through", got.Config.Effort)
	}
}

// Duplicate keys keep encoding/json's last-wins behaviour: the request is
// parseable, so it is not the gateway's business to reject it.
func TestWebSearchDuplicateKeysLastWins(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{}
	rec := postSearch(t, newSearchServer(t, se), `{"query":"first","query":"second"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if got := se.lastRequest(); got.Query != "second" {
		t.Fatalf("query = %q, want the last value", got.Query)
	}
}

func TestWebSearchMethodNotAllowed(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/v1/web_search", nil)
	rec := httptest.NewRecorder()
	newSearchServer(t, &fakeSearcher{}).Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", got)
	}
}

func TestWebSearchEmptyResults(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{fn: func(int) (*upstream.SearchResult, error) {
		return &upstream.SearchResult{}, nil
	}}
	rec := postSearch(t, newSearchServer(t, se), `{"query":"something obscure"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decodeSearch(t, rec)
	// Empty slices must serialise as [] so a client never has to handle null.
	if !strings.Contains(rec.Body.String(), `"queries":[]`) || !strings.Contains(rec.Body.String(), `"results":[]`) {
		t.Fatalf("empty arrays not serialised as []: %s", rec.Body)
	}
	if body["object"] != "web_search" {
		t.Fatalf("object = %v", body["object"])
	}
	if se.count() != 1 {
		t.Fatalf("empty results triggered %d runs, want exactly 1 (no retries)", se.count())
	}
}

func TestWebSearchUpstreamErrorMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"pool empty", upstream.ErrUnavailable, http.StatusServiceUnavailable, openai.CodeUpstreamUnavailable},
		{"idle", upstream.ErrIdleTimeout, http.StatusGatewayTimeout, openai.CodeUpstreamIdle},
		{"incomplete", upstream.ErrSearchIncomplete, http.StatusBadGateway, openai.CodeUpstreamStream},
		{"status 429", &upstream.StatusError{Status: 429, Body: "challenge"}, http.StatusBadGateway, openai.CodeUpstreamError},
		{"search error event", &upstream.SearchError{Message: "backend down"}, http.StatusBadGateway, openai.CodeUpstreamError},
		{"transport", errors.New("dial failed"), http.StatusBadGateway, openai.CodeUpstreamError},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			se := &fakeSearcher{fn: func(int) (*upstream.SearchResult, error) { return nil, tc.err }}
			rec := postSearch(t, newSearchServer(t, se), `{"query":"q"}`)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body)
			}
			var env openai.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("envelope: %v", err)
			}
			if env.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q", env.Error.Code, tc.code)
			}
			// Upstream prose is translated, not hidden, but an HTML
			// challenge page must never be echoed.
			if strings.Contains(rec.Body.String(), "<html") {
				t.Fatalf("upstream HTML leaked: %s", rec.Body)
			}
			switch tc.name {
			case "search error event":
				if !strings.Contains(rec.Body.String(), "backend down") {
					t.Fatalf("upstream message not translated: %s", rec.Body)
				}
			case "pool empty", "idle", "incomplete", "transport":
				if strings.Contains(rec.Body.String(), "backend down") {
					t.Fatal("unrelated error leaked a message")
				}
			}
		})
	}
}

func TestWebSearchConfigDisabledIs503(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{cfg: &upstream.SearchConfigResponse{RetrievalAvailable: false}}
	s := newSearchServer(t, se)
	s.probeSearch(context.Background())

	rec := postSearch(t, s, `{"query":"q"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var env openai.ErrorResponse
	json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != openai.CodeSearchUnavailable {
		t.Fatalf("code = %q", env.Error.Code)
	}
	if se.count() != 0 {
		t.Fatal("a disabled deployment still ran a search")
	}
}

// A failed config probe leaves the endpoint usable and the health value null:
// the probe is an optimisation, not a gate.
func TestWebSearchProbeFailureDoesNotDisable(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{
		cfgFn: func(context.Context) (*upstream.SearchConfigResponse, error) {
			return nil, errors.New("challenge")
		},
		fn: func(int) (*upstream.SearchResult, error) { return &upstream.SearchResult{}, nil },
	}
	s := newSearchServer(t, se)
	s.probeSearch(context.Background())
	if rec := postSearch(t, s, `{"query":"q"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, probe failure must not disable search", rec.Code)
	}
	if got := s.SearchAvailable(); got != nil {
		t.Fatalf("search available = %v, want nil after a failed probe", *got)
	}
}

func TestHealthzReportsSearchAvailability(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cfg  *upstream.SearchConfigResponse
		err  error
		want string
	}{
		"available":    {cfg: &upstream.SearchConfigResponse{RetrievalAvailable: true}, want: `"available":true`},
		"unavailable":  {cfg: &upstream.SearchConfigResponse{RetrievalAvailable: false}, want: `"available":false`},
		"probe failed": {err: errors.New("challenge"), want: `"available":null`},
	} {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			se := &fakeSearcher{cfg: tc.cfg}
			if tc.err != nil {
				se.cfgFn = func(context.Context) (*upstream.SearchConfigResponse, error) { return nil, tc.err }
			}
			s := newSearchServer(t, se)
			s.probeSearch(context.Background())
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("healthz = %s, want %s", rec.Body, tc.want)
			}
		})
	}
}

func TestHealthzSearchNullBeforeProbe(t *testing.T) {
	t.Parallel()
	s := newSearchServer(t, &fakeSearcher{})
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"available":null`) {
		t.Fatalf("healthz = %s", rec.Body)
	}
}

// Without a searcher the route answers 503 rather than panicking; this is the
// degraded mode if the upstream client cannot be wired at boot.
func TestWebSearchWithoutSearcherIs503(t *testing.T) {
	t.Parallel()
	s := New(Config{Model: "mercury-3", Logf: func(string, ...any) {}}, nil, nil)
	rec := postSearch(t, s, `{"query":"q"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestWebSearchAuthRequired(t *testing.T) {
	t.Parallel()
	s := newSearchServer(t, &fakeSearcher{})
	s.cfg.APIKey = "k"
	rec := postSearch(t, s, `{"query":"q"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// Guard: the config probe is launched in the background and must not delay
// handler construction, and a slow probe must not block /v1/web_search.
func TestProbeDoesNotBlockStartup(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	se := &fakeSearcher{
		cfgFn: func(ctx context.Context) (*upstream.SearchConfigResponse, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return &upstream.SearchConfigResponse{RetrievalAvailable: true}, nil
		},
		fn: func(int) (*upstream.SearchResult, error) { return &upstream.SearchResult{}, nil },
	}
	s := newSearchServer(t, se)
	s.cfg.SearchProbeTimeout = 50 * time.Millisecond
	done := make(chan struct{})
	go func() { s.StartSearchProbe(); close(done) }()
	// The endpoint answers while the probe is still hung.
	start := time.Now()
	if rec := postSearch(t, s, `{"query":"q"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("endpoint waited %s on the config probe", elapsed)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("probe bounded by its timeout did not return")
	}
}

// Compile-time assertions for the interfaces the handlers depend on.
var (
	_ Chatter  = (*fakeChatter)(nil)
	_ Searcher = (*fakeSearcher)(nil)
)

// The chat stream never reproduces search sentinels: a `source-url` titled
// __search_error__ must not reach the client as content or a citation, while a
// real source still becomes an annotation.
func TestChatStreamDropsSearchSentinels(t *testing.T) {
	t.Parallel()
	body := `data: {"type":"text-start","id":"text-0"}
data: {"type":"source-url","sourceId":"s0","url":"","title":"__searching__"}
data: {"type":"source-url","sourceId":"s1","url":"","title":"__search_error__"}
data: {"type":"source-url","sourceId":"s2","url":"https://real.example","title":"Real"}
data: {"type":"text-delta","id":"text-0","delta":"answer"}
data: {"type":"text-end","id":"text-0"}
data: [DONE]
`
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(body), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	out := rec.Body.String()
	for _, sentinel := range []string{"__searching__", "__search_error__"} {
		if strings.Contains(out, sentinel) {
			t.Fatalf("chat stream leaked %q: %s", sentinel, out)
		}
	}
	if !strings.Contains(out, `"content":"answer"`) {
		t.Fatalf("content lost: %s", out)
	}
	// The real source survives as a citation, ranked after the sentinels.
	if !strings.Contains(out, `"type":"url_citation"`) || !strings.Contains(out, `"index":1`) || !strings.Contains(out, "real.example") {
		t.Fatalf("real source lost its citation: %s", out)
	}
}

// The upstream body is exactly four keys; the gateway must not smuggle chat
// fields (messages, model, system) into the retrieval request.
func TestWebSearchUpstreamBodyShape(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{}
	postSearch(t, newSearchServer(t, se), `{"query":"q"}`)
	req := se.lastRequest()
	if req.Query != "q" || req.Config.Fanout != 3 || req.Timezone != "UTC" || req.Language != "en-US" {
		t.Fatalf("request = %+v", req)
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"query"`, `"config"`, `"timezone"`, `"language"`} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("missing %s in %s", key, b)
		}
	}
	for _, key := range []string{`"messages"`, `"model"`, `"system"`, `"webSearchEnabled"`} {
		if strings.Contains(string(b), key) {
			t.Fatalf("unexpected %s in %s", key, b)
		}
	}
}

// Trailing data is unparseable and rejected. Duplicate keys keep
// encoding/json's last-wins meaning and are forwarded: the JSON parser's
// contract is the upstream's contract too.
func TestWebSearchRejectsTrailingDataOnly(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"query":"q"} garbage`,
		`{"query":"q"}{"query":"q2"}`,
	} {
		se := &fakeSearcher{}
		rec := postSearch(t, newSearchServer(t, se), body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rec.Code)
		}
		if se.count() != 0 {
			t.Errorf("body %s reached upstream", body)
		}
	}
	// A config array is structurally wrong (an object is required).
	se := &fakeSearcher{}
	if rec := postSearch(t, newSearchServer(t, se), `{"query":"q","config":[]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("config array: status = %d, want 400", rec.Code)
	}
}

func TestWebSearchSetsNoStore(t *testing.T) {
	t.Parallel()
	rec := postSearch(t, newSearchServer(t, &fakeSearcher{}), `{"query":"q"}`)
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

// A single failure is counted once: searchError must not increment
// upstream_errors and then delegate to upstreamError, which increments again.
func TestWebSearchCountsUpstreamErrorOnce(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{fn: func(int) (*upstream.SearchResult, error) {
		return nil, upstream.ErrIdleTimeout
	}}
	s := newSearchServer(t, se)
	rec := postSearch(t, s, `{"query":"q"}`)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := s.upErrors.Load(); got != 1 {
		t.Fatalf("upstream_errors = %d, want 1", got)
	}
}

// A Searcher returning (nil, nil) must not panic the probe goroutine.
func TestProbeNilConfigDoesNotPanic(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{cfgFn: func(context.Context) (*upstream.SearchConfigResponse, error) {
		return nil, nil
	}}
	s := newSearchServer(t, se)
	s.probeSearch(context.Background())
	if got := s.SearchAvailable(); got != nil {
		t.Fatalf("available = %v, want nil", *got)
	}
}

// An explicit "config": null is the same as omitting it.
func TestWebSearchNullConfigUsesDefaults(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{}
	rec := postSearch(t, newSearchServer(t, se), `{"query":"q","config":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := se.lastRequest(); got.Config.Fanout != 3 || got.Config.Effort != "instant" {
		t.Fatalf("defaults = %+v", got.Config)
	}
}
