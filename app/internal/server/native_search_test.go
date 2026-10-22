package server

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/upstream"
)

// newNativeServer builds a tool-test server with the native web_search tool
// enabled (or not) and a scripted retrieval backend. The clock is fixed so the
// result's as_of date is deterministic.
func newNativeServer(t *testing.T, up Chatter, se Searcher, enabled bool) *Server {
	t.Helper()
	s := New(Config{
		Model:           "mercury-3",
		Models:          []string{"mercury-3"},
		IdleTimeout:     60 * time.Second,
		NativeWebSearch: enabled,
		Logf:            func(string, ...any) {},
	}, up, fakePool{})
	if se != nil {
		s.SetSearcher(se)
	}
	s.SetClock(func() time.Time { return time.Unix(1_792_553_057, 0) })
	return s
}

// upstreamTexts flattens every message of a captured upstream request.
func upstreamTexts(req *upstream.ChatRequest) []string {
	out := make([]string, 0, len(req.Messages))
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			out = append(out, p.Text)
		}
	}
	return out
}

func preambleOf(req *upstream.ChatRequest) string {
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" && len(req.Messages[0].Parts) > 0 {
		return req.Messages[0].Parts[0].Text
	}
	return ""
}

var reToolResponse = regexp.MustCompile(`(?s)<tool_response[^>]*>\n(.*)\n</tool_response>`)

// toolResponses returns every tool result body carried by a request.
func toolResponses(req *upstream.ChatRequest) []string {
	var out []string
	for _, text := range upstreamTexts(req) {
		for _, m := range reToolResponse.FindAllStringSubmatch(text, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

func searchCall(query string) string {
	return `<tool_call>{"name":"web_search","arguments":{"query":` + mustJSON(query) + `}}</tool_call>`
}

// Native search off (the default): the preamble carries only the client's
// tools and a web_search block is prose, never a call.
func TestNativeSearchOffDoesNotDeclare(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) {
		return streamOf(toolStream(searchCall("x"))), nil
	}}
	se := &fakeSearcher{}
	s := newNativeServer(t, up, se, false)
	rec := postChat(t, s, toolBody(true, ""))
	if strings.Contains(preambleOf(up.lastRequest()), "web_search") {
		t.Fatalf("native off declared web_search:\n%s", preambleOf(up.lastRequest()))
	}
	if strings.Contains(rec.Body.String(), `"tool_calls"`) {
		t.Fatalf("undeclared call became a tool frame: %s", rec.Body)
	}
	if se.count() != 0 {
		t.Fatalf("search executed with native off: %d", se.count())
	}
}

// Native search on with client tools that do not name web_search: the
// preamble carries the gateway's definition with a real parameter schema, and
// the tool is declared to the parser (so a call is never prose).
func TestNativeSearchDeclaresTool(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(toolStream("ok")), nil }}
	s := newNativeServer(t, up, &fakeSearcher{}, true)
	postChat(t, s, toolBody(false, ""))
	pre := preambleOf(up.lastRequest())
	if !strings.Contains(pre, `"name":"web_search"`) || !strings.Contains(pre, `"parameters":{"type":"object"`) {
		t.Fatalf("native tool not declared with a schema:\n%s", pre)
	}
	if !strings.Contains(pre, `"name":"get_weather"`) {
		t.Fatalf("client tool lost:\n%s", pre)
	}
}

// A request without tools never gets the native tool, even when enabled.
func TestNativeSearchNeedsClientTools(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(toolStream("hi")), nil }}
	s := newNativeServer(t, up, &fakeSearcher{}, true)
	postChat(t, s, `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	for _, text := range upstreamTexts(up.lastRequest()) {
		if strings.Contains(text, "web_search") {
			t.Fatalf("native tool leaked into a tool-less request: %q", text)
		}
	}
}

// A client that declares its own web_search owns the name: no auto-declare,
// no interception, and the call passes through untouched.
func TestNativeSearchClientOwnsName(t *testing.T) {
	t.Parallel()
	clientTool := `{"type":"function","function":{"name":"web_search","description":"client search","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}`
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) {
		return streamOf(toolStream(`<tool_call>{"name":"web_search","arguments":{"q":"x"}}</tool_call>`)), nil
	}}
	se := &fakeSearcher{}
	s := newNativeServer(t, up, se, true)
	body := `{"model":"mercury-3","stream":true,"tools":[` + clientTool + `],"messages":[{"role":"user","content":"q"}]}`
	rec := postChat(t, s, body)

	pre := preambleOf(up.lastRequest())
	if strings.Count(pre, `"name":"web_search"`) != 1 || !strings.Contains(pre, "client search") {
		t.Fatalf("client definition was not preserved verbatim:\n%s", pre)
	}
	if se.count() != 0 {
		t.Fatalf("client-owned call was intercepted: %d searches", se.count())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) || !strings.Contains(out, `"name":"web_search"`) {
		t.Fatalf("client call did not pass through: %s", out)
	}
}

// Happy path: the model's web_search call is served internally, its result is
// replayed as a tool response, and only the final answer reaches the client.
func TestNativeSearchHappyPath(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{fn: func(int) (*upstream.SearchResult, error) {
		return &upstream.SearchResult{
			Queries: []string{"capital of France"},
			Sources: []upstream.SearchSource{{Title: "France", URL: "https://example.com/fr", Score: 0.9}},
		}, nil
	}}
	up := &fakeChatter{fn: func(call int) (*upstream.Stream, error) {
		if call == 1 {
			return streamOf(toolStream(searchCall("capital of France"))), nil
		}
		return streamOf(toolStream("Paris is the capital.")), nil
	}}
	s := newNativeServer(t, up, se, true)
	rec := postChat(t, s, toolBody(true, ""))

	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	if se.count() != 1 {
		t.Fatalf("searches = %d, want 1", se.count())
	}
	if got := se.lastRequest(); got.Query != "capital of France" || got.Config.Fanout != 3 ||
		got.Config.Depth != 1 || got.Config.KeepTopK != 10 || got.Config.Effort != "instant" {
		t.Fatalf("search request = %+v", got)
	}

	// The server must re-enter the upstream with the assistant call and the
	// result turn appended.
	if up.count() != 2 {
		t.Fatalf("upstream rounds = %d, want 2", up.count())
	}
	second := up.lastRequest()
	texts := upstreamTexts(second)
	if !strings.Contains(strings.Join(texts, "\n"), `"name":"web_search"`) {
		t.Fatalf("assistant call not replayed: %v", texts)
	}
	responses := toolResponses(second)
	if len(responses) != 1 {
		t.Fatalf("tool responses = %v", responses)
	}
	var payload struct {
		Object  string `json:"object"`
		AsOf    string `json:"as_of"`
		Queries []string
		Results []struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(responses[0]), &payload); err != nil {
		t.Fatalf("result is not JSON: %v (%q)", err, responses[0])
	}
	if payload.Object != "web_search" || payload.AsOf != "2026-10-21" {
		t.Fatalf("result envelope = %+v", payload)
	}
	if len(payload.Results) != 1 || payload.Results[0].URL != "https://example.com/fr" {
		t.Fatalf("results = %+v", payload.Results)
	}

	out := rec.Body.String()
	if !strings.Contains(out, "Paris is the capital.") {
		t.Fatalf("final answer missing: %s", out)
	}
	if strings.Contains(out, `"name":"web_search"`) || strings.Contains(out, "tool_response") {
		t.Fatalf("internal tool traffic leaked: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("finish = %s", out)
	}
}

// The budget caps executed searches at three; the next call is answered with
// the budget error and the model gets to wrap up.
func TestNativeSearchBudget(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{}
	up := &fakeChatter{fn: func(call int) (*upstream.Stream, error) {
		if call <= 4 {
			return streamOf(toolStream(searchCall("q"))), nil
		}
		return streamOf(toolStream("done")), nil
	}}
	s := newNativeServer(t, up, se, true)
	rec := postChat(t, s, toolBody(true, ""))

	if se.count() != 3 {
		t.Fatalf("searches = %d, want 3", se.count())
	}
	if up.count() != 5 {
		t.Fatalf("upstream rounds = %d, want 5 (4 calls + final)", up.count())
	}
	responses := toolResponses(up.lastRequest())
	if len(responses) != 4 {
		t.Fatalf("tool responses = %d, want 4", len(responses))
	}
	if !strings.Contains(responses[3], "search budget exhausted") {
		t.Fatalf("fourth result = %q", responses[3])
	}
	if !strings.Contains(rec.Body.String(), "done") || strings.Contains(rec.Body.String(), `"name":"web_search"`) {
		t.Fatalf("body = %s", rec.Body)
	}
}

// A backend failure rides the tool result; the loop still reaches the final
// answer.
func TestNativeSearchFailurePassesThrough(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{fn: func(int) (*upstream.SearchResult, error) {
		return nil, &upstream.SearchError{Message: "retrieval exploded"}
	}}
	up := &fakeChatter{fn: func(call int) (*upstream.Stream, error) {
		if call == 1 {
			return streamOf(toolStream(searchCall("q"))), nil
		}
		return streamOf(toolStream("answered anyway")), nil
	}}
	s := newNativeServer(t, up, se, true)
	rec := postChat(t, s, toolBody(true, ""))

	responses := toolResponses(up.lastRequest())
	if len(responses) != 1 || !strings.Contains(responses[0], `"error"`) || !strings.Contains(responses[0], "retrieval exploded") {
		t.Fatalf("tool result = %v", responses)
	}
	if !strings.Contains(rec.Body.String(), "answered anyway") || !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Fatalf("body = %s", rec.Body)
	}
}

// A local backend that is not configured means the tool is never declared.
func TestNativeSearchWithoutBackendDoesNotDeclare(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(toolStream("ok")), nil }}
	s := newNativeServer(t, up, nil, true)
	postChat(t, s, toolBody(true, ""))
	if strings.Contains(preambleOf(up.lastRequest()), "web_search") {
		t.Fatalf("declared without a backend:\n%s", preambleOf(up.lastRequest()))
	}
}

// Non-streaming requests run the same loop and return only the final answer.
func TestNativeSearchNonStreaming(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{}
	up := &fakeChatter{fn: func(call int) (*upstream.Stream, error) {
		if call == 1 {
			return streamOf(toolStream(searchCall("q"))), nil
		}
		return streamOf(toolStream("final")), nil
	}}
	s := newNativeServer(t, up, se, true)
	rec := postChat(t, s, toolBody(false, ""))
	var c openai.Completion
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	ch := c.Choices[0]
	if ch.FinishReason != "stop" || ch.Message.Content != "final" || len(ch.Message.ToolCalls) != 0 {
		t.Fatalf("choice = %+v", ch)
	}
	if se.count() != 1 {
		t.Fatalf("searches = %d", se.count())
	}
}

// A transport-level backend failure is not a tool result: it surfaces as an
// OpenAI error frame.
func TestNativeSearchBackendUnavailableIsErrorFrame(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{fn: func(int) (*upstream.SearchResult, error) {
		return nil, errors.New("dial tcp: connection refused")
	}}
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) {
		return streamOf(toolStream(searchCall("q"))), nil
	}}
	s := newNativeServer(t, up, se, true)
	rec := postChat(t, s, toolBody(true, ""))
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("no error frame: %s", rec.Body)
	}
}

// The empty-answer failure mode can land on the final native round. Nothing
// has been written yet, so the same one-shot retry applies and no frame from
// the dead round may reach the client.
func TestNativeSearchEmptyFinalRoundRetriedOnce(t *testing.T) {
	t.Parallel()
	empty := "data: {\"type\":\"reasoning-start\",\"id\":\"r0\"}\n\n" +
		"data: {\"type\":\"reasoning-end\",\"id\":\"r0\"}\n\n" +
		"data: [DONE]\n\n"
	se := &fakeSearcher{}
	up := &fakeChatter{fn: func(call int) (*upstream.Stream, error) {
		switch call {
		case 1:
			return streamOf(toolStream(searchCall("q"))), nil
		case 2:
			return streamOf(empty), nil
		default:
			return streamOf(toolStream("second try")), nil
		}
	}}
	s := newNativeServer(t, up, se, true)
	rec := postChat(t, s, toolBody(true, ""))
	if up.count() != 3 {
		t.Fatalf("upstream rounds = %d, want 3 (search, empty, retry)", up.count())
	}
	if !strings.Contains(rec.Body.String(), "second try") || !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Fatalf("retry not passed through: %s", rec.Body)
	}
}

// A model that keeps calling web_search past the budget is stopped by the
// round limit instead of looping forever.
func TestNativeSearchRoundLimit(t *testing.T) {
	t.Parallel()
	se := &fakeSearcher{}
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) {
		return streamOf(toolStream(searchCall("q"))), nil
	}}
	s := newNativeServer(t, up, se, true)
	rec := postChat(t, s, toolBody(true, ""))
	if se.count() != nativeSearchBudget {
		t.Fatalf("searches = %d, want %d", se.count(), nativeSearchBudget)
	}
	if up.count() != nativeSearchBudget+2 {
		t.Fatalf("rounds = %d, want %d", up.count(), nativeSearchBudget+2)
	}
	// The loop is stopped, not aborted: a normal stop with no leaked frames.
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"name":"web_search"`) ||
		strings.Contains(rec.Body.String(), "tool_response") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Fatalf("finish missing: %s", rec.Body)
	}
}
