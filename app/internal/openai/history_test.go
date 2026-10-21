package openai

import (
	"encoding/json"
	"testing"
)

func TestNormalizeHistoryMatchesNormalizeForPlainMessages(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: []any{map[string]any{"type": "text", "text": "a"}, map[string]any{"type": "image_url"}}},
		{Role: "assistant", Content: "b"},
		{Role: "weird", Content: "c"},
	}
	plain, plainTotal, re := NormalizeMessages(msgs)
	if re != nil {
		t.Fatalf("NormalizeMessages: %+v", re)
	}
	hist, histTotal, re := NormalizeHistory(msgs)
	if re != nil {
		t.Fatalf("NormalizeHistory: %+v", re)
	}
	if plainTotal != histTotal || len(plain) != len(hist) {
		t.Fatalf("plain=%v/%d hist=%v/%d", plain, plainTotal, hist, histTotal)
	}
	for i := range plain {
		if plain[i].Role != hist[i].Role {
			t.Errorf("%d: role %q != %q", i, plain[i].Role, hist[i].Role)
		}
		if len(plain[i].Texts) != len(hist[i].Texts) {
			t.Errorf("%d: texts %v != %v", i, plain[i].Texts, hist[i].Texts)
		}
		for j := range plain[i].Texts {
			if plain[i].Texts[j] != hist[i].Texts[j] {
				t.Errorf("%d/%d: %q != %q", i, j, plain[i].Texts[j], hist[i].Texts[j])
			}
		}
		if hist[i].Calls != nil && len(hist[i].Calls) > 0 {
			t.Errorf("%d: unexpected calls", i)
		}
	}
}

func TestNormalizeHistoryExtractsCallsAndResults(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		{Role: "user", Content: "weather?"},
		{Role: "assistant", Content: nil, ToolCalls: json.RawMessage(`[{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]`)},
		{Role: "tool", ToolCallID: json.RawMessage(`"call_a"`), Content: `{"tempC":21}`},
		{Role: "tool", ToolCallID: json.RawMessage(`"call_b"`), Content: ""},
	}
	out, _, re := NormalizeHistory(msgs)
	if re != nil {
		t.Fatalf("NormalizeHistory: %+v", re)
	}
	// An assistant message with only tool_calls has no text; it must not be a
	// 400 either.
	if len(out) != 4 {
		t.Fatalf("out = %+v", out)
	}
	if len(out[1].Calls) != 1 || out[1].Calls[0].ID != "call_a" {
		t.Fatalf("calls = %+v", out[1].Calls)
	}
	if out[2].Results[0].CallID != "call_a" || out[2].Results[0].Content != `{"tempC":21}` {
		t.Fatalf("result = %+v", out[2].Results)
	}
	if out[3].Results[0].Content != "" {
		t.Fatalf("empty result = %+v", out[3].Results)
	}
}

// A message with a tool role but no content is still a result, never a 400.
func TestNormalizeHistoryToolWithoutContent(t *testing.T) {
	t.Parallel()
	_, _, re := NormalizeHistory([]Message{{Role: "tool", ToolCallID: json.RawMessage(`"c"`)}})
	if re != nil {
		t.Fatalf("tool result rejected: %+v", re)
	}
}

func TestNormalizeHistoryDropsBadCalls(t *testing.T) {
	t.Parallel()
	cases := []string{
		`not json`,
		`null`,
		`[]`,
		`[{"function":{"name":"f"}}]`, // no id
		`[{"id":"call_a","function":{"name":""}}]`,             // no name
		`[{"id":"call_a","function":{"name":"f"}},{"id":"x"}]`, // mixed: keeps one
	}
	want := []int{0, 0, 0, 0, 0, 1}
	for i, raw := range cases {
		out, _, re := NormalizeHistory([]Message{{Role: "assistant", Content: "x", ToolCalls: json.RawMessage(raw)}})
		if re != nil {
			t.Errorf("%s: %+v", raw, re)
			continue
		}
		if len(out[0].Calls) != want[i] {
			t.Errorf("%s: calls = %+v, want %d", raw, out[0].Calls, want[i])
		}
	}
}

func TestNormalizeHistoryBadToolCallID(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`123`, `{"a":1}`, `null`, ``} {
		out, _, re := NormalizeHistory([]Message{{Role: "tool", ToolCallID: json.RawMessage(raw), Content: "x"}})
		if re != nil {
			t.Errorf("%s: %+v", raw, re)
			continue
		}
		if out[0].Results[0].CallID != "" {
			t.Errorf("%s: call id = %q", raw, out[0].Results[0].CallID)
		}
	}
}
