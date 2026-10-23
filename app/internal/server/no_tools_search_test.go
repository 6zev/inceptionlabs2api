package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/upstream"
)

// T2 (SDD-refusal-recovery §1): a chat request with NO client tools must also
// benefit from the gateway's native web_search — the model calls the
// gateway-declared tool, the search is served internally, and the client sees
// only the final answer. The boundaries keep byte-identical legacy behavior:
// tool_choice:"none" never activates, and NativeWebSearch=off (or no
// searcher) keeps the plain path.

// noToolsRequest builds the production request shape that surfaced the
// canned-refusal failure: a plain chat turn with no tools array.
func noToolsRequest(prompt string, stream bool) []byte {
	body := map[string]any{
		"model":    "mercury",
		"stream":   stream,
		"messages": []map[string]any{{"role": "user", "content": prompt}},
	}
	b, _ := json.Marshal(body)
	return b
}

// artexSearchRound scripts the model's internal web_search call for the
// production prompt.
func artexSearchRound() string {
	return sseCallRaw("web_search", `{"query":"GitHub artex 二开 增强版本"}`)
}

// noToolsSearchFlow scripts the full two-round native loop for the no-tools
// path: the search call, then the grounded answer.
func noToolsSearchFlow() (*mockChatter, *mockSearcher) {
	final := "Artex 的二开增强版本包括 artdex 与 artex-plus [1][2]。"
	return &mockChatter{rounds: []string{artexSearchRound(), sseText(final)}},
		&mockSearcher{runs: []*upstream.SearchResult{go127Run()}}
}

func TestNoToolsChatActivatesNativeSearchNonStream(t *testing.T) {
	mock, se := noToolsSearchFlow()
	s := newNativeServer(t, mock, se)
	w := postChat(t, s, noToolsRequest("github上搜索artex，寻找都有哪些二开增强版本", false))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if got := mock.count(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (search round + answer round)", got)
	}

	// The FIRST upstream request must carry the auto-declared manifest: a
	// system preamble and webSearchEnabled:false (the measured mutual
	// exclusion — the native tool is served from our manifest alone).
	first := mock.request(0)
	if first.WebSearchEnabled {
		t.Fatalf("no-tools manifest round must set webSearchEnabled:false")
	}
	if len(first.Messages) == 0 || first.Messages[0].Role != "system" ||
		!strings.Contains(strings.Join(partTexts(first.Messages[0]), ""), "web_search") {
		t.Fatalf("first round must open with a web_search-bearing preamble: %+v", first.Messages[0])
	}

	// The SECOND upstream request must carry the tool result with as_of, so
	// the model can reason about recency.
	second := mock.request(1)
	sawAsOf := false
	for _, m := range second.Messages {
		if strings.Contains(strings.Join(partTexts(m), ""), `"as_of"`) {
			sawAsOf = true
		}
	}
	if !sawAsOf {
		t.Fatalf("second round must carry the search result with as_of: %+v", second.Messages)
	}

	var comp openai.Completion
	if err := json.Unmarshal(w.Body.Bytes(), &comp); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if got := comp.Choices[0].Message.Content; !strings.Contains(got, "artex-plus") {
		t.Fatalf("final content = %q, want the grounded answer", got)
	}
	// The internal web_search call is digested by the gateway: the client
	// never sees a tool_calls frame.
	if len(comp.Choices[0].Message.ToolCalls) != 0 {
		t.Fatalf("native call leaked to client: %+v", comp.Choices[0].Message.ToolCalls)
	}
	// The retrieved sources ride as annotations.
	cites := comp.Choices[0].Message.Annotations
	if len(cites) != 2 || cites[0].URL != "https://go.dev/doc/go1.27" {
		t.Fatalf("annotations = %+v, want the two search sources", cites)
	}
}

func TestNoToolsChatToolChoiceNoneStaysLegacy(t *testing.T) {
	// tool_choice:"none" is the client's explicit opt-out: the request must
	// take the legacy path byte-for-byte — one round, webSearchEnabled:true.
	prose := "Artex 是一个开源项目。"
	mock := &mockChatter{rounds: []string{sseText(prose)}}
	s := newNativeServer(t, mock, &mockSearcher{})
	body := map[string]any{
		"model":       "mercury",
		"messages":    []map[string]any{{"role": "user", "content": "什么是artex"}},
		"tool_choice": "none",
	}
	b, _ := json.Marshal(body)
	w := postChat(t, s, b)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if got := mock.count(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (tool_choice none must not activate)", got)
	}
	if !mock.request(0).WebSearchEnabled {
		t.Fatalf("tool_choice:none must keep webSearchEnabled:true (legacy)")
	}
	var comp openai.Completion
	if err := json.Unmarshal(w.Body.Bytes(), &comp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if comp.Choices[0].Message.Content != prose {
		t.Fatalf("content = %q, want verbatim %q", comp.Choices[0].Message.Content, prose)
	}
}

func TestNoToolsChatNativeOffStaysLegacy(t *testing.T) {
	// The master switch stays NativeWebSearch: off means the no-tools request
	// is byte-identical legacy — one round, webSearchEnabled:true.
	prose := "我无法回答这个问题。"
	mock := &mockChatter{rounds: []string{sseText(prose)}}
	s := newTestServer(t, mock) // NativeWebSearch unset, no searcher
	w := postChat(t, s, noToolsRequest("github上搜索artex", false))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if got := mock.count(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (native off keeps legacy)", got)
	}
	if !mock.request(0).WebSearchEnabled {
		t.Fatalf("native off must keep webSearchEnabled:true (legacy)")
	}
	var comp openai.Completion
	if err := json.Unmarshal(w.Body.Bytes(), &comp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if comp.Choices[0].Message.Content != prose {
		t.Fatalf("content = %q, want verbatim %q", comp.Choices[0].Message.Content, prose)
	}
}

func TestNoToolsChatActivatesNativeSearchStream(t *testing.T) {
	mock, se := noToolsSearchFlow()
	s := newNativeServer(t, mock, se)
	w := postChat(t, s, noToolsRequest("github上搜索artex，寻找都有哪些二开增强版本", true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if got := mock.count(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	chunks := streamFrames(t, w.Body.String())
	if got := contentOf(chunks); !strings.Contains(got, "artex-plus") {
		t.Fatalf("streamed content = %q, want the grounded answer", got)
	}
	// No internal round leaks: the client stream never carries a tool_calls
	// frame, and the search narration frame never appears.
	if names := toolCallNames(chunks); len(names) != 0 {
		t.Fatalf("internal web_search call leaked to client stream: %v", names)
	}
	// The retrieved sources ride the finish frame as annotations.
	var cites []openai.Citation
	for _, c := range chunks {
		cites = append(cites, c.Choices[0].Delta.Annotations...)
	}
	if len(cites) != 2 || cites[0].URL != "https://go.dev/doc/go1.27" {
		t.Fatalf("streamed annotations = %+v, want the two search sources", cites)
	}
}
