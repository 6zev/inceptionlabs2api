package server

import (
	"encoding/json"
	"testing"

	"inceptionlabs2api/internal/openai"
)

func msg(role string, calls ...string) openai.Message {
	m := openai.Message{Role: role, Content: role + " text"}
	if len(calls) > 0 {
		var raw []map[string]any
		for _, c := range calls {
			raw = append(raw, map[string]any{
				"id":       "call_" + c,
				"type":     "function",
				"function": map[string]any{"name": c, "arguments": "{}"},
			})
		}
		b, _ := json.Marshal(raw)
		m.ToolCalls = b
	}
	return m
}

func roles(ms []openai.Message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Role
	}
	return out
}

func TestTrimNoopUnderCap(t *testing.T) {
	in := []openai.Message{msg("system"), msg("user"), msg("assistant")}
	got, dropped := trimMessages(in, 10)
	if dropped != 0 || len(got) != len(in) {
		t.Fatalf("expected no trim, got dropped=%d len=%d", dropped, len(got))
	}
}

func TestTrimDisabledWhenMaxZero(t *testing.T) {
	in := make([]openai.Message, 200)
	for i := range in {
		in[i] = msg("user")
	}
	got, dropped := trimMessages(in, 0)
	if dropped != 0 || len(got) != 200 {
		t.Fatalf("max=0 must disable trim, got dropped=%d len=%d", dropped, len(got))
	}
}

func TestTrimKeepsSystemAndNewest(t *testing.T) {
	in := []openai.Message{msg("system")}
	for i := 0; i < 20; i++ {
		in = append(in, msg("user"), msg("assistant"))
	}
	got, dropped := trimMessages(in, 10)
	if dropped == 0 {
		t.Fatal("expected a trim")
	}
	if got[0].Role != "system" {
		t.Fatalf("system turn lost: %v", roles(got))
	}
	if len(got) > 10 {
		t.Fatalf("still over cap: %d", len(got))
	}
	if got[len(got)-1].Role != "assistant" {
		t.Fatalf("newest turn lost: %v", roles(got))
	}
}

func TestTrimNeverSplitsToolGroup(t *testing.T) {
	// system, then alternating, then a trailing assistant(call)+tool pair that
	// must never be separated.
	in := []openai.Message{msg("system")}
	for i := 0; i < 6; i++ {
		in = append(in, msg("user"), msg("assistant"))
	}
	callIdx := len(in)
	in = append(in, msg("assistant", "get_weather"), msg("tool"))
	got, _ := trimMessages(in, 5)

	// If the assistant-with-calls survived, its tool result must survive too.
	foundCall, foundResult := false, false
	for i, m := range got {
		if hasToolCalls(m) {
			foundCall = true
			if i+1 < len(got) && got[i+1].Role == "tool" {
				foundResult = true
			}
		}
	}
	if foundCall != foundResult {
		t.Fatalf("tool group split: %v", roles(got))
	}
	if !foundCall {
		t.Logf("tool group trimmed wholesale (acceptable), ctx=%d", callIdx)
	}
}

func TestTrimStartsWithUser(t *testing.T) {
	in := []openai.Message{msg("system"), msg("assistant"), msg("user"), msg("assistant"), msg("user"), msg("assistant")}
	got, _ := trimMessages(in, 4)
	if len(got) > 1 && got[1].Role != "user" && got[1].Role != "tool" {
		t.Fatalf("expected a user turn right after system, got %v", roles(got))
	}
}
