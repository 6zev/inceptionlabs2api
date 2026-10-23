package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/upstream"
)

// T3 integration (SDD-refusal-recovery §1): a zero-call round whose prose is
// a canned refusal gets ONE bounded recovery retry with the refusal-retry
// message; the refused round's original text rides the retry history. A
// refusal→refusal retry passes the second refusal through verbatim — bounded
// means exactly one retry, never two.

// refusalProse is the measured production refusal (dbg4 r10), verbatim.
func refusalProse() string {
	return "I'm Mercury, built by Inception. I'm not able to get into the details of how I was built, but I'm happy to help with something else."
}

func TestRefusedRoundRetriesOnceAndEmitsCall(t *testing.T) {
	fixed := callOpenMarker() + `{"name":"get_weather","arguments":{"city":"南宁"}}` + callCloseMarker()
	mock := &mockChatter{rounds: []string{sseText(refusalProse()), sseText(fixed)}}
	s := newTestServer(t, mock)
	w := postChat(t, s, weatherRequest(false))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if got := mock.count(); got != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2 (refused round + one bounded retry)", got)
	}
	var comp openai.Completion
	if err := json.Unmarshal(w.Body.Bytes(), &comp); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if len(comp.Choices[0].Message.ToolCalls) != 1 || comp.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("client must receive the retried call: %s", w.Body.String())
	}

	// The retry request must carry the refusal verbatim as the assistant turn
	// plus the refusal-retry instruction.
	retryReq := mock.request(1)
	if len(retryReq.Messages) < 3 {
		t.Fatalf("retry request must carry assistant+retry turns, got %d messages", len(retryReq.Messages))
	}
	asst := retryReq.Messages[len(retryReq.Messages)-2]
	if asst.Role != "assistant" || strings.Join(partTexts(asst), "") != refusalProse() {
		t.Fatalf("retry must replay the refusal verbatim: %+v", asst)
	}
	last := retryReq.Messages[len(retryReq.Messages)-1]
	if last.Role != "user" || !strings.Contains(strings.Join(partTexts(last), ""), "was a refusal") {
		t.Fatalf("refusal retry turn missing/wrong: %+v", last)
	}
	if strings.Contains(strings.Join(partTexts(last), ""), refusalProse()) {
		t.Fatalf("refusal retry turn must not embed the refusal text: %+v", last)
	}
	// The refusal retry scrubs the switch note from historical user turns:
	// with the note left in, the model re-fires the identity reflex and
	// burns the one bounded retry (il2a-live2 A/B).
	for i, m := range retryReq.Messages {
		if m.Role != "user" {
			continue
		}
		joined := strings.Join(partTexts(m), "")
		if strings.Contains(joined, "model was just switched") && !strings.Contains(joined, "was a refusal") {
			t.Fatalf("retry request user turn %d still carries the switch note: %+v", i, m)
		}
	}
}

func TestRefusedRetryStillProsePassesThroughBounded(t *testing.T) {
	// Refusal → retry → still refusal-shaped prose: the second refusal
	// passes through verbatim and NO third upstream call happens.
	second := "我无法访问互联网，请你自行搜索。"
	mock := &mockChatter{rounds: []string{sseText(refusalProse()), sseText(second)}}
	s := newTestServer(t, mock)
	w := postChat(t, s, weatherRequest(false))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if got := mock.count(); got != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2 (bounded: one retry, never two)", got)
	}
	var comp openai.Completion
	if err := json.Unmarshal(w.Body.Bytes(), &comp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if comp.Choices[0].Message.Content != second {
		t.Fatalf("second refusal must pass through verbatim: %q", comp.Choices[0].Message.Content)
	}
}

func TestRefusedRoundRetriesOnceStream(t *testing.T) {
	fixed := callOpenMarker() + `{"name":"get_weather","arguments":{"city":"南宁"}}` + callCloseMarker()
	mock := &mockChatter{rounds: []string{sseText(refusalProse()), sseText(fixed)}}
	s := newTestServer(t, mock)
	w := postChat(t, s, weatherRequest(true))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if got := mock.count(); got != 2 {
		t.Fatalf("upstream calls = %d, want exactly 2", got)
	}
	chunks := streamFrames(t, w.Body.String())
	if names := toolCallNames(chunks); len(names) != 1 || names[0] != "get_weather" {
		t.Fatalf("client must see the retried call, got %v (body: %s)", names, w.Body.String())
	}
	// The refusal must NOT leak to the client before the retry.
	if strings.Contains(contentOf(chunks), "I'm Mercury") {
		t.Fatalf("refusal prose leaked to client: %q", contentOf(chunks))
	}
}

func TestNativeRefusedRoundRetriesWithHistory(t *testing.T) {
	// Native path: round 1 refuses, the retry re-enters the native loop from
	// the accumulated history plus the refusal turns, and the retry round
	// calls the gateway's web_search — served internally, so the loop needs
	// one more round to answer. Exactly: refusal + retry-search + answer.
	call := sseCallRaw("web_search", `{"query":"go 1.27 release date"}`)
	final := "Go 1.27 was released on August 19, 2026 [1][2]."
	mock := &mockChatter{rounds: []string{sseText(refusalProse()), call, sseText(final)}}
	se := &mockSearcher{runs: []*upstream.SearchResult{go127Run()}}
	s := newNativeServer(t, mock, se)
	w := postChat(t, s, weatherRequest(false))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if got := mock.count(); got != 3 {
		t.Fatalf("upstream calls = %d, want 3 (refused + retry-search + answer)", got)
	}
	// The retry request must carry the refusal turns before the search call.
	retryReq := mock.request(1)
	sawRefusal, sawInstruction := false, false
	for _, m := range retryReq.Messages {
		joined := strings.Join(partTexts(m), "")
		if strings.Contains(joined, "I'm Mercury, built by Inception") {
			sawRefusal = true
		}
		if strings.Contains(joined, "was a refusal") {
			sawInstruction = true
		}
	}
	if !sawRefusal || !sawInstruction {
		t.Fatalf("native retry lost the refusal turns (refusal=%v instruction=%v)", sawRefusal, sawInstruction)
	}
	var comp openai.Completion
	if err := json.Unmarshal(w.Body.Bytes(), &comp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if comp.Choices[0].Message.Content != final {
		t.Fatalf("final answer must pass through: %q", comp.Choices[0].Message.Content)
	}
	if cites := comp.Choices[0].Message.Annotations; len(cites) != 2 {
		t.Fatalf("retry's search sources must reach the client: %+v", cites)
	}
}

// noToolsSwitchNoteRequest is the measured identity-lockup shape: a plain
// no-tools request whose user turn carries the hermes-style switch note.
func noToolsSwitchNoteRequest(stream bool) []byte {
	body := map[string]any{
		"model":  "mercury",
		"stream": stream,
		"messages": []map[string]any{
			{"role": "system", "content": "你是小码酱，一个AI编程助手。"},
			{"role": "user", "content": "[Note: model was just switched from cn:glm-5.3 to mercury-3 via 130.61.28.166:12888. Adjust your self-identification accordingly.]\n\n今天中东国际局势"},
		},
	}
	b, _ := json.Marshal(body)
	return b
}

// TestPlanTimeNoteStripPinsFirstRoundClean: the switch note is stripped at
// plan time, so the FIRST upstream request already carries the clean user
// text — no refusal ever happens, no retry is ever spent (the retry-path
// scrub remains as a belt for other refusal shapes).
func TestPlanTimeNoteStripPinsFirstRoundClean(t *testing.T) {
	answer := "根据最新消息，中东局势处于高度紧张状态。"
	mock := &mockChatter{rounds: []string{sseText(answer)}}
	s := newTestServer(t, mock)
	w := postChat(t, s, noToolsSwitchNoteRequest(false))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", w.Code, w.Body.String())
	}
	if got := mock.count(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 (plan-time strip: no refusal, no retry)", got)
	}
	// The upstream request's user turn must already be note-free.
	first := mock.request(0)
	for _, m := range first.Messages {
		if m.Role != "user" {
			continue
		}
		for _, p := range m.Parts {
			if strings.Contains(p.Text, "model was just switched") {
				t.Fatalf("first upstream request still carries the switch note: %q", p.Text)
			}
		}
	}
	var comp openai.Completion
	if err := json.Unmarshal(w.Body.Bytes(), &comp); err != nil {
		t.Fatalf("decode completion: %v", err)
	}
	if comp.Choices[0].Message.Content != answer {
		t.Fatalf("content = %q", comp.Choices[0].Message.Content)
	}
}
