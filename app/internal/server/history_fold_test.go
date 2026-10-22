package server

import (
	"encoding/json"
	"strings"
	"testing"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/tools"
	"inceptionlabs2api/internal/upstream"
)

// foldMessages builds n user/assistant message pairs for a request body.
func foldMessages(n int) []map[string]any {
	msgs := make([]map[string]any, 0, 2*n)
	for i := 0; i < n; i++ {
		msgs = append(msgs,
			map[string]any{"role": "user", "content": "question " + itoaFold(i)},
			map[string]any{"role": "assistant", "content": "answer " + itoaFold(i)},
		)
	}
	return msgs
}

func itoaFold(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// foldToolSchema is the canonical get_weather schema, one raw JSON value so
// the request fixture and the preamble expectation render identically (a
// map[string]any would re-order keys under json.Marshal).
const foldToolSchema = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`

// foldChatBody builds a non-streaming chat request body with optional tools.
func foldChatBody(msgs []map[string]any, withTools bool) []byte {
	body := map[string]any{
		"model":    "mercury",
		"stream":   false,
		"messages": msgs,
	}
	if withTools {
		body["tools"] = []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "Get current weather",
				"parameters":  json.RawMessage(foldToolSchema),
			},
		}}
	}
	b, _ := json.Marshal(body)
	return b
}

// textOf joins an upstream message's text parts into one string.
func textOf(m upstream.Message) string {
	return strings.Join(partTexts(m), "\n")
}

// buildPreamble renders the manifest preamble the way planTools does, so the
// tests can byte-compare against the wire.
func buildPreamble(t *testing.T) string {
	t.Helper()
	m, err := tools.BuildManifest([]tools.Tool{{
		Name:        "get_weather",
		Description: "Get current weather",
		Parameters:  json.RawMessage(foldToolSchema),
	}}, tools.Choice{Auto: true}, false)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	return m.Preamble
}

// bubbleCount counts transcript-bubble messages in one upstream request.
func bubbleCount(req *upstream.ChatRequest) (int, string) {
	n, text := 0, ""
	for _, m := range req.Messages {
		t := ""
		for _, p := range m.Parts {
			t += p.Text
		}
		if strings.Contains(t, "[The following is a verbatim transcript") {
			n++
			text = t
		}
	}
	return n, text
}

// --- Server-level test 1: below trigger, the wire is untouched ---

func TestFoldWiringBelowTriggerUnchanged(t *testing.T) {
	// Plain path: exactly 80 messages, one scripted round.
	mock := &mockChatter{rounds: []string{sseText("hi")}}
	s := New(Config{Model: "mercury", Logf: func(string, ...any) {}}, mock, nil)
	postChat(t, s, foldChatBody(foldMessages(40), false))
	if mock.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", mock.count())
	}
	ureq := mock.request(0)
	if len(ureq.Messages) != 80 {
		t.Fatalf("80-message plain request must cross unchanged, got %d messages", len(ureq.Messages))
	}
	if got := textOf(ureq.Messages[0]); got != "question 0" {
		t.Fatalf("first message changed: %q", got)
	}

	// Tools path: 79 history + 1 preamble = 80 upstream, no fold, and the
	// preamble stays byte-identical to a plain manifest build.
	mock = &mockChatter{rounds: []string{sseText("hi"), sseText("hi")}}
	s = New(Config{Model: "mercury", Logf: func(string, ...any) {}}, mock, nil)
	postChat(t, s, foldChatBody(append(foldMessages(39)[:78:78],
		map[string]any{"role": "user", "content": "last"}), true))
	if mock.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", mock.count())
	}
	ureq = mock.request(0)
	if len(ureq.Messages) != 80 {
		t.Fatalf("79+1 tools request must cross unchanged, got %d messages", len(ureq.Messages))
	}
	if got := textOf(ureq.Messages[0]); got != buildPreamble(t) {
		t.Fatalf("unfolded preamble drifted from the plain manifest build")
	}
	if strings.Contains(textOf(ureq.Messages[0]), openai.HistoryFormatSection) {
		t.Fatalf("unfolded preamble must not contain the history-format section")
	}
}

// --- Server-level test 6: preamble teaching section, conditional ---

func TestFoldPreambleTeachingSection(t *testing.T) {
	// Folded tools request: the preamble gains the section; the bubble does
	// not carry it (tool path teaches in the preamble only).
	mock := &mockChatter{rounds: []string{sseText("hi")}}
	s := New(Config{Model: "mercury", Logf: func(string, ...any) {}}, mock, nil)
	postChat(t, s, foldChatBody(foldMessages(43), true)) // 86 history + preamble
	if mock.count() != 1 {
		t.Fatalf("upstream calls = %d, want 1", mock.count())
	}
	ureq := mock.request(0)
	if len(ureq.Messages) > 60 {
		t.Fatalf("folded tools request has %d messages, want <= 60", len(ureq.Messages))
	}
	preamble := textOf(ureq.Messages[0])
	if preamble != buildPreamble(t)+"\n\n"+openai.HistoryFormatSection+"\n" {
		t.Fatalf("folded preamble is not the base + history-format section")
	}
	n, bubble := bubbleCount(ureq)
	if n != 1 {
		t.Fatalf("folded request has %d bubbles, want 1", n)
	}
	if strings.Contains(bubble, openai.HistoryFormatSection) {
		t.Fatalf("tool-path bubble must not duplicate the preamble's teaching section")
	}
	if !strings.Contains(bubble, "question 3") {
		t.Fatalf("bubble lost old content")
	}
}

// --- Server-level plain-path wiring ---

func TestFoldWiringPlainPath(t *testing.T) {
	mock := &mockChatter{rounds: []string{sseText("hi")}}
	s := New(Config{Model: "mercury", Logf: func(string, ...any) {}}, mock, nil)
	postChat(t, s, foldChatBody(foldMessages(50), false)) // 100 > 80
	ureq := mock.request(0)
	if len(ureq.Messages) > 60 {
		t.Fatalf("folded plain request has %d messages, want <= 60", len(ureq.Messages))
	}
	n, bubble := bubbleCount(ureq)
	if n != 1 {
		t.Fatalf("folded plain request has %d bubbles, want 1", n)
	}
	// The plain path teaches the markers inside the bubble (no preamble).
	if !strings.Contains(bubble, openai.HistoryFormatSection) {
		t.Fatalf("plain-path bubble missing the history-format section")
	}
	// The recent tail rides as real messages.
	sawFinal := false
	for _, m := range ureq.Messages {
		if textOf(m) == "answer 49" {
			sawFinal = true
		}
	}
	if !sawFinal {
		t.Fatalf("recent tail lost the newest answer")
	}
}

// --- Server-level test 7: the native loop never re-folds ---

func TestFoldNativeLoopNoDoubleFold(t *testing.T) {
	// 86 history messages + tools; the native web_search tool is on. The
	// model calls web_search once, then answers. Every internal re-ask must
	// carry the SAME folded bubble — the loop appends its rounds to the
	// folded history and folding never runs again.
	se := &mockSearcher{runs: []*upstream.SearchResult{{
		Queries: []string{"q"},
		Sources: []upstream.SearchSource{{Title: "T", URL: "https://x", Score: 1}},
	}}}
	mock := &mockChatter{rounds: []string{
		sseCall("web_search", `{"query":"latest"}`),
		sseText("final answer"),
	}}
	s := newNativeServer(t, mock, se)
	postChat(t, s, foldChatBody(foldMessages(43), true))
	if mock.count() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (search round + answer round)", mock.count())
	}

	first, second := mock.request(0), mock.request(1)
	n1, bubble1 := bubbleCount(first)
	n2, bubble2 := bubbleCount(second)
	if n1 != 1 || n2 != 1 {
		t.Fatalf("bubbles: first=%d second=%d, want 1 each (no re-fold)", n1, n2)
	}
	if bubble1 != bubble2 {
		t.Fatalf("the loop re-folded: bubble changed between rounds")
	}
	if len(first.Messages) > 60 {
		t.Fatalf("first folded request has %d messages, want <= 60", len(first.Messages))
	}
	// The second round adds exactly the assistant call turn + the result
	// group — growth is the loop's own append, not a fold.
	if len(second.Messages) != len(first.Messages)+2 {
		t.Fatalf("second round = %d messages, want first+2 (%d)", len(second.Messages), len(first.Messages)+2)
	}
	// The preamble carries the teaching section on every round.
	for i, req := range []*upstream.ChatRequest{first, second} {
		if !strings.Contains(textOf(req.Messages[0]), openai.HistoryFormatSection) {
			t.Fatalf("round %d preamble missing the history-format section", i)
		}
	}
}
