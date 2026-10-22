package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"inceptionlabs2api/internal/upstream"
)

// The wire-format call markers are assembled from split literals: the raw
// sequences are meaningful to the surrounding tooling and must not appear
// contiguously in this file.
func callOpenMarker() string  { return "<tool_" + "call>" }
func callCloseMarker() string { return "</tool" + "_call>" }

// sseCall renders one marker-protocol tool call as an SSE round body.
func sseCall(name, args string) string {
	return sseText(callOpenMarker() + `{"name":"` + name + `","arguments":` + args + `}` + callCloseMarker())
}

// mockSearcher scripts one SearchResult per queued run and records requests.
type mockSearcher struct {
	mu    sync.Mutex
	runs  []*upstream.SearchResult
	errs  []error
	reqs  []*upstream.SearchRequest
	slept time.Duration // unused; kept nil-safe
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

// Unused import guards for helpers shared with later components.
var (
	_ io.Reader     = strings.NewReader("")
	_ time.Duration = time.Second
	_ http.Handler  = http.DefaultServeMux
)
