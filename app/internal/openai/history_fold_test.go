package openai

import (
	"strings"
	"testing"
)

// mkPairs builds n plain user/assistant pairs, i.e. 2n messages.
func mkPairs(n int) []History {
	out := make([]History, 0, 2*n)
	for i := 0; i < n; i++ {
		out = append(out,
			History{Role: "user", Texts: []string{strings.Repeat("u", i+1) + " user message " + itoa(i)}},
			History{Role: "assistant", Texts: []string{"assistant answer " + itoa(i)}},
		)
	}
	return out
}

// probeText renders every piece of a History a content search could look at:
// its texts, its tool result bodies and its tool call names.
func probeText(h History) string {
	parts := append([]string{}, h.Texts...)
	for _, r := range h.Results {
		parts = append(parts, r.Content)
	}
	for _, c := range h.Calls {
		parts = append(parts, c.Function.Name)
	}
	return strings.Join(parts, "\n")
}

// --- Test 1: below trigger, nothing changes ---

func TestFoldBelowTriggerUnchanged(t *testing.T) {
	// Plain path: exactly 80 history messages, no tools placeholder.
	hist := mkPairs(40)
	folded, did := FoldHistory(hist, false)
	if did {
		t.Fatalf("80-message plain history must not fold")
	}
	if len(folded) != len(hist) {
		t.Fatalf("folded length = %d, want %d", len(folded), len(hist))
	}
	for i := range hist {
		if folded[i].Role != hist[i].Role || strings.Join(folded[i].Texts, "\n") != strings.Join(hist[i].Texts, "\n") {
			t.Fatalf("message %d changed below trigger", i)
		}
	}

	// Tools path: 79 history + 1 preamble placeholder = 80 upstream, no fold.
	hist = mkPairs(39)
	hist = append(hist, History{Role: "user", Texts: []string{"last"}})
	if len(hist) != 79 {
		t.Fatalf("fixture must be 79 messages, got %d", len(hist))
	}
	folded, did = FoldHistory(hist, true)
	if did {
		t.Fatalf("79+1 = 80 upstream count must not fold")
	}
	if len(folded) != 79 {
		t.Fatalf("below-trigger tools history changed: %d", len(folded))
	}
}

// --- Test 2: 101 messages fold to bubble + tail, <= 60, content preserved ---

func TestFold101Messages(t *testing.T) {
	hist := mkPairs(50) // 100
	hist = append(hist, History{Role: "user", Texts: []string{"final question"}})
	folded, did := FoldHistory(hist, false)
	if !did {
		t.Fatalf("101-message history must fold")
	}
	if len(folded) > 60 {
		t.Fatalf("folded history has %d messages, want <= 60", len(folded))
	}
	// The first message must be the transcript bubble: a user message whose
	// content carries the bracket header and ChatML markers.
	if folded[0].Role != "user" || len(folded[0].Texts) != 1 {
		t.Fatalf("folded[0] is not a single-text user bubble: %+v", folded[0])
	}
	bubble := folded[0].Texts[0]
	if !strings.Contains(bubble, "[The following is a verbatim transcript") {
		t.Fatalf("bubble missing transcript header")
	}
	if !strings.Contains(bubble, "<|im_start|>user") || !strings.Contains(bubble, "<|im_end|>") {
		t.Fatalf("bubble missing ChatML markers")
	}
	// Old content is present verbatim (nothing hostile here, no truncation).
	for _, probe := range []string{"user message 3", "assistant answer 17", "user message 39"} {
		if !strings.Contains(bubble, probe) {
			t.Fatalf("bubble lost old content %q", probe)
		}
	}
	// The recent tail keeps the newest turns verbatim as real messages.
	var sawFinal, sawLastAnswer bool
	for _, h := range folded[1:] {
		if h.Role == "user" && strings.Join(h.Texts, "") == "final question" {
			sawFinal = true
		}
		if strings.Join(h.Texts, "") == "assistant answer 49" {
			sawLastAnswer = true
		}
	}
	if !sawFinal || !sawLastAnswer {
		t.Fatalf("recent tail lost the newest turns")
	}
}

// --- Test 3: atomic tool groups at the fold boundary ---

func TestFoldAtomicToolGroups(t *testing.T) {
	// Sub-case A: the tool exchange group lands in the recent tail. It must
	// travel whole — call and result together, untouched.
	hist := mkPairs(40)
	hist = append(hist,
		History{Role: "user", Texts: []string{"boundary question"}},
		History{Role: "assistant", Calls: []ToolCall{{
			ID: "call_tail", Type: "function",
			Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"Nanning"}`},
		}}},
		History{Role: "user", Results: []ToolResult{{CallID: "call_tail", Content: `{"temp":31}`}}},
	)
	if len(hist) != 83 {
		t.Fatalf("fixture must be 83 messages, got %d", len(hist))
	}
	folded, did := FoldHistory(hist, false)
	if !did {
		t.Fatalf("83-message history must fold")
	}
	assertGroupAtomic(t, folded, "get_weather", `"temp":31`)

	// Sub-case B: the same group lands in the old prefix. It must render
	// whole into the bubble.
	hist = mkPairs(30)
	hist = append(hist,
		History{Role: "user", Texts: []string{"boundary question"}},
		History{Role: "assistant", Calls: []ToolCall{{
			ID: "call_bubble", Type: "function",
			Function: FunctionCall{Name: "get_weather", Arguments: `{"city":"Nanning"}`},
		}}},
		History{Role: "user", Results: []ToolResult{{CallID: "call_bubble", Content: `{"temp":31}`}}},
	)
	hist = append(hist, mkPairs(10)...) // 20 more
	hist = append(hist, History{Role: "user", Texts: []string{"final"}})
	folded, did = FoldHistory(hist, false)
	if !did {
		t.Fatalf("84-message history must fold")
	}
	assertGroupAtomic(t, folded, "get_weather", `"temp":31`)
}

// assertGroupAtomic verifies the tool call and its result live on the same
// side of the fold: both in the bubble, or both in the tail.
func assertGroupAtomic(t *testing.T, folded []History, callProbe, resultProbe string) {
	t.Helper()
	callInBubble := strings.Contains(folded[0].Texts[0], callProbe)
	resultInBubble := strings.Contains(folded[0].Texts[0], resultProbe)
	for _, h := range folded[1:] {
		if strings.Contains(probeText(h), callProbe) {
			callInBubble = false
		}
		if strings.Contains(probeText(h), resultProbe) {
			resultInBubble = false
		}
	}
	if callInBubble != resultInBubble {
		t.Fatalf("tool group split across the fold boundary: call in bubble=%v, result in bubble=%v",
			callInBubble, resultInBubble)
	}
}

// --- Test 8: leading system messages never fold ---

func TestFoldSystemNeverFolded(t *testing.T) {
	hist := []History{
		{Role: "system", Texts: []string{"sys one"}},
		{Role: "system", Texts: []string{"sys two"}},
	}
	hist = append(hist, mkPairs(50)...) // 100 more => 102 total
	folded, did := FoldHistory(hist, false)
	if !did {
		t.Fatalf("102-message history must fold")
	}
	if folded[0].Role != "system" || folded[0].Texts[0] != "sys one" {
		t.Fatalf("first system message folded: %+v", folded[0])
	}
	if folded[1].Role != "system" || folded[1].Texts[0] != "sys two" {
		t.Fatalf("second system message folded: %+v", folded[1])
	}
	if folded[2].Role != "user" {
		t.Fatalf("bubble must follow the system messages, got role %q", folded[2].Role)
	}
	if !strings.Contains(folded[2].Texts[0], "[The following is a verbatim transcript") {
		t.Fatalf("folded[2] is not the bubble")
	}
}

// --- Test 9: pathological tail alone > 60 → extra folding, nothing dropped ---

func TestFoldKeepsAllMessages(t *testing.T) {
	// Five plain pairs, then ONE giant turn-group: a user question followed
	// by 35 assistant-with-calls/result pairs (71 messages in a single
	// group). 81 messages total, so the trigger fires and the newest group —
	// which alone exceeds the 60-message cap — must be partially folded at
	// ATOMIC unit granularity: whole groups first, then leading units, never
	// splitting an assistant-with-calls from its results, never dropping.
	hist := mkPairs(5) // 10
	hist = append(hist, History{Role: "user", Texts: []string{"giant question"}})
	for i := 0; i < 35; i++ {
		id := "call_g" + itoa(i)
		hist = append(hist,
			History{Role: "assistant", Calls: []ToolCall{{
				ID: id, Type: "function",
				Function: FunctionCall{Name: "tool" + itoa(i), Arguments: "{}"},
			}}},
			History{Role: "user", Results: []ToolResult{{CallID: id, Content: `{"i":` + itoa(i) + `}`}}},
		)
	}
	if len(hist) != 81 {
		t.Fatalf("fixture must be 81 messages, got %d", len(hist))
	}

	folded, did := FoldHistory(hist, false)
	if !did {
		t.Fatalf("81-message history must fold")
	}
	if len(folded) > 60 {
		t.Fatalf("pathological fold produced %d messages, want <= 60", len(folded))
	}
	// Nothing dropped: every call name appears in the bubble or the tail.
	for i := 0; i < 35; i++ {
		probe := "tool" + itoa(i)
		found := strings.Contains(folded[0].Texts[0], probe)
		for _, h := range folded[1:] {
			if strings.Contains(probeText(h), probe) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("message with %s was dropped", probe)
		}
	}
	// Atomicity survived the unit split: each result body sits on the same
	// side as its assistant call (the bubble renders both; the tail keeps
	// both as History entries).
	assertGroupAtomic(t, folded, "tool0", `{"i":0}`)
	assertGroupAtomic(t, folded, "tool34", `{"i":34}`)
}
