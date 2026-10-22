package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/upstream"
)

// The wire-format call markers are assembled from split literals: the raw
// sequences are meaningful to the surrounding tooling and must not appear
// contiguously in this file.
func callOpenMarker() string  { return "<tool_" + "call>" }
func callCloseMarker() string { return "</tool" + "_call>" }

// sseCallRaw renders one marker-protocol tool call as an SSE round body,
// without JSON HTML escaping (see below).
func sseCall(name, args string) string {
	return sseCallRaw(name, args)
}

// mockSearcher scripts one SearchResult per queued run and records requests.
type mockSearcher struct {
	mu   sync.Mutex
	runs []*upstream.SearchResult
	errs []error
	reqs []*upstream.SearchRequest
}

func (m *mockSearcher) Search(ctx context.Context, req *upstream.SearchRequest, idle time.Duration) (*upstream.SearchResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reqs = append(m.reqs, req)
	if len(m.errs) > 0 {
		err := m.errs[0]
		m.errs = m.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(m.runs) == 0 {
		return nil, fmt.Errorf("mock: no scripted search run left")
	}
	res := m.runs[0]
	m.runs = m.runs[1:]
	return res, nil
}

func (m *mockSearcher) SearchConfig(ctx context.Context) (*upstream.SearchConfigResponse, error) {
	return &upstream.SearchConfigResponse{}, nil
}

func (m *mockSearcher) requestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.reqs)
}

// go127Run is the captured Go 1.27 retrieval: two sources and the synthesized
// answer with citation markers aligned to the results order.
func go127Run() *upstream.SearchResult {
	return &upstream.SearchResult{
		Queries: []string{"go 1.27 release date", "go 1.27 new features"},
		Sources: []upstream.SearchSource{
			{Title: "Go 1.27 Release Notes", URL: "https://go.dev/doc/go1.27", Score: 0.97},
			{Title: "The Go Blog: Go 1.27", URL: "https://go.dev/blog/go1.27", Score: 0.91},
		},
		Answer: "Go 1.27 was released on August 19, 2026 [1][2].",
	}
}

// newNativeServer wires a Server with the native web_search tool on and the
// mock retrieval backend attached.
func newNativeServer(t *testing.T, mock *mockChatter, se Searcher) *Server {
	t.Helper()
	s := New(Config{Model: "mercury", NativeWebSearch: true, Logf: func(string, ...any) {}}, mock, nil)
	s.SetSearcher(se)
	return s
}

// --- Component 2: the answer reaches the model and /v1/web_search ---

func TestNativeSearchContentIncludesAnswer(t *testing.T) {
	body := nativeSearchContent(go127Run(), "2026-10-22")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("tool result is not JSON: %v (%s)", err, body)
	}
	if got, want := decoded["answer"], "Go 1.27 was released on August 19, 2026 [1][2]."; got != want {
		t.Fatalf("answer = %v, want %q", got, want)
	}
	results, ok := decoded["results"].([]any)
	if !ok || len(results) != 2 {
		t.Fatalf("results missing/wrong: %s", body)
	}
	first, _ := results[0].(map[string]any)
	if first["cite_index"].(float64) != 1 {
		t.Fatalf("cite_index must start at 1 in results order: %s", body)
	}
}

func TestNativeSearchContentOmitsEmptyAnswer(t *testing.T) {
	res := go127Run()
	res.Answer = ""
	body := nativeSearchContent(res, "2026-10-22")
	if strings.Contains(body, `"answer"`) {
		t.Fatalf("empty answer must omit the field entirely: %s", body)
	}
}

func TestWebSearchEndpointReturnsAnswer(t *testing.T) {
	se := &mockSearcher{runs: []*upstream.SearchResult{go127Run()}}
	s := newNativeServer(t, &mockChatter{}, se)

	req := httptest.NewRequest(http.MethodPost, "/v1/web_search", strings.NewReader(`{"query":"go 1.27 new features"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	var resp webSearchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Answer != "Go 1.27 was released on August 19, 2026 [1][2]." {
		t.Fatalf("answer = %q, want the captured synthesized answer", resp.Answer)
	}
	if resp.Object != "web_search" || len(resp.Results) != 2 || len(resp.Queries) != 2 {
		t.Fatalf("existing fields must be unchanged: %s", w.Body.String())
	}
	if resp.Results[0].CiteIndex != 1 || resp.Results[0].URL != "https://go.dev/doc/go1.27" {
		t.Fatalf("results must keep their order: %+v", resp.Results)
	}
}

func TestWebSearchEndpointOmitsAnswerWhenUpstreamHasNone(t *testing.T) {
	res := go127Run()
	res.Answer = ""
	se := &mockSearcher{runs: []*upstream.SearchResult{res}}
	s := newNativeServer(t, &mockChatter{}, se)

	req := httptest.NewRequest(http.MethodPost, "/v1/web_search", strings.NewReader(`{"query":"go 1.27"}`))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"answer"`) {
		t.Fatalf("no answer upstream means no answer field: %s", w.Body.String())
	}
}

// --- Component 3: native search sources reach the client as annotations ---

// nativeFlow scripts a two-round native search: the model calls web_search,
// then answers. Returns the mock chatter and the searcher.
func nativeFlow() (*mockChatter, *mockSearcher) {
	search := sseCallRaw("web_search", `{"query":"go 1.27 release date"}`)
	final := "Go 1.27 was released on August 19, 2026 [1][2]."
	return &mockChatter{rounds: []string{search, sseText(final)}}, &mockSearcher{runs: []*upstream.SearchResult{go127Run()}}
}

func TestNativeSearchSourcesBecomeAnnotationsNonStream(t *testing.T) {
	mock, se := nativeFlow()
	s := newNativeServer(t, mock, se)
	w := postChat(t, s, weatherRequest(false))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	var comp openai.Completion
	if err := json.Unmarshal(w.Body.Bytes(), &comp); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	cites := comp.Choices[0].Message.Annotations
	if len(cites) != 2 {
		t.Fatalf("annotations = %+v, want the two search sources", cites)
	}
	if cites[0].URL != "https://go.dev/doc/go1.27" || cites[0].Type != "url_citation" || cites[0].Index != 1 {
		t.Fatalf("first citation wrong: %+v", cites[0])
	}
	if cites[1].URL != "https://go.dev/blog/go1.27" || cites[1].Index != 2 {
		t.Fatalf("second citation wrong: %+v", cites[1])
	}
	if cites[0].Title != "Go 1.27 Release Notes" {
		t.Fatalf("title must pass through: %+v", cites[0])
	}
	if comp.Choices[0].Message.Content != "Go 1.27 was released on August 19, 2026 [1][2]." {
		t.Fatalf("final answer must pass through: %q", comp.Choices[0].Message.Content)
	}
}

func TestNativeSearchSourcesBecomeAnnotationsStream(t *testing.T) {
	mock, se := nativeFlow()
	s := newNativeServer(t, mock, se)
	w := postChat(t, s, weatherRequest(true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	var got []openai.Citation
	for _, c := range streamFrames(t, w.Body.String()) {
		got = append(got, c.Choices[0].Delta.Annotations...)
	}
	if len(got) != 2 || got[0].URL != "https://go.dev/doc/go1.27" || got[1].URL != "https://go.dev/blog/go1.27" {
		t.Fatalf("streamed annotations = %+v, want the two search sources", got)
	}
}

func TestNoNativeSearchMeansNoAnnotations(t *testing.T) {
	// Native search off: even though the round is identical, no source
	// events fire on the tool path and no annotations may appear.
	mock, _ := nativeFlow()
	s := newTestServer(t, mock) // NativeWebSearch unset
	w := postChat(t, s, weatherRequest(false))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	var comp openai.Completion
	if err := json.Unmarshal(w.Body.Bytes(), &comp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cites := comp.Choices[0].Message.Annotations; len(cites) != 0 {
		t.Fatalf("plain path must not gain annotations: %+v", cites)
	}
}

func TestNativeSearchDedupesSourcesAcrossSearches(t *testing.T) {
	// Two searches returning one shared and one distinct source each:
	// annotations carry three citations, first-seen order, deduped by URL.
	shared := "https://go.dev/doc/go1.27"
	s1 := &upstream.SearchResult{Sources: []upstream.SearchSource{
		{Title: "Go 1.27 Release Notes", URL: shared, Score: 0.97},
		{Title: "Go Blog A", URL: "https://go.dev/blog/a", Score: 0.9},
	}}
	s2 := &upstream.SearchResult{Sources: []upstream.SearchSource{
		{Title: "Go 1.27 Release Notes (dup)", URL: shared, Score: 0.5},
		{Title: "Go Blog B", URL: "https://go.dev/blog/b", Score: 0.8},
	}}
	call1 := sseCallRaw("web_search", `{"query":"go 1.27"}`)
	call2 := sseCallRaw("web_search", `{"query":"go 1.27 features"}`)
	final := "done"
	mock := &mockChatter{rounds: []string{call1, call2, sseText(final)}}
	se := &mockSearcher{runs: []*upstream.SearchResult{s1, s2}}
	s := newNativeServer(t, mock, se)
	w := postChat(t, s, weatherRequest(false))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	var comp openai.Completion
	if err := json.Unmarshal(w.Body.Bytes(), &comp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cites := comp.Choices[0].Message.Annotations
	if len(cites) != 3 {
		t.Fatalf("annotations = %+v, want 3 deduped citations", cites)
	}
	if cites[0].URL != shared || cites[1].URL != "https://go.dev/blog/a" || cites[2].URL != "https://go.dev/blog/b" {
		t.Fatalf("first-seen order broken: %+v", cites)
	}
}

// sseCallRaw is sseCall without JSON HTML escaping: the wire format carries
// the call markers as raw bytes, and encoding/json's default < > escaping
// would corrupt them into </> forms the parser never sees.
func sseCallRaw(name, args string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	payload := map[string]any{"name": name, "arguments": json.RawMessage(args)}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	_ = enc.Encode(upstream.Event{Type: upstream.EventTextStart, Snapshot: callOpenMarker() + string(raw) + callCloseMarker()})
	return "data: " + strings.TrimRight(b.String(), "\n") + "\n\ndata: [DONE]\n\n"
}
