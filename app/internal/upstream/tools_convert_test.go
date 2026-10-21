package upstream

import (
	"encoding/json"
	"regexp"
	"testing"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/tools"
)

// T1: the tool-less path must produce the same body it produced before v8.
// NewChatRequest is unchanged; NewToolChatRequest with no tool material must
// fall through to it.
func TestToolChatRequestWithoutToolsMatchesLegacy(t *testing.T) {
	t.Parallel()
	msgs := []openai.TextMessage{
		{Role: "system", Texts: []string{"be brief"}},
		{Role: "user", Texts: []string{"hello"}},
		{Role: "assistant", Texts: []string{"hi"}},
	}
	legacy := NewChatRequest(msgs, "low")
	tool := NewToolChatRequest(msgs, "low", nil)

	lb := marshalNormalised(t, legacy)
	tb := marshalNormalised(t, tool)
	if lb != tb {
		t.Fatalf("tool-less body differs:\nlegacy %s\ntool   %s", lb, tb)
	}
}

// With a manifest, one system message carrying the preamble precedes the
// client's messages, and nothing else changes.
func TestToolChatRequestPrependsPreamble(t *testing.T) {
	t.Parallel()
	msgs := []openai.TextMessage{{Role: "user", Texts: []string{"weather?"}}}
	m, err := tools.BuildManifest([]tools.Tool{{Name: "get_weather", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}}, tools.Choice{Auto: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	req := NewToolChatRequest(msgs, "", m)
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Parts[0].Text != m.Preamble {
		t.Fatalf("first message = %+v", req.Messages[0])
	}
	if req.Messages[0].Parts[0].State != "" {
		t.Fatal("system part must have no state")
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Parts[0].Text != "weather?" {
		t.Fatalf("second message = %+v", req.Messages[1])
	}
}

// Replay: assistant calls and grouped tool results become the encoded text.
func TestToolChatRequestReplaysHistory(t *testing.T) {
	t.Parallel()
	hist := []openai.History{
		{Role: "user", Texts: []string{"weather in Paris?"}},
		{Role: "assistant", Calls: []openai.ToolCall{{ID: "call_a", Function: openai.FunctionCall{Name: "get_weather", Arguments: `{"city":"Paris"}`}}}},
		{Role: "user", Results: []openai.ToolResult{{CallID: "call_a", Content: `{"tempC":21}`}}},
		{Role: "user", Results: []openai.ToolResult{{CallID: "call_b", Content: `{"tempC":9}`}}},
	}
	req := NewToolChatRequestHistory(hist, "low", nil)
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d: %+v", len(req.Messages), req.Messages)
	}
	asst := req.Messages[1].Parts[0].Text
	if !regexp.MustCompile(`<tool_call>\{"id":"call_a","name":"get_weather","arguments":\{"city":"Paris"\}\}</tool_call>`).MatchString(asst) {
		t.Fatalf("assistant replay = %q", asst)
	}
	grouped := req.Messages[2].Parts[0].Text
	if req.Messages[2].Role != "user" {
		t.Fatalf("results role = %q", req.Messages[2].Role)
	}
	for _, want := range []string{`call_id="call_a"`, `call_id="call_b"`, "21", "9"} {
		if !contains(grouped, want) {
			t.Fatalf("grouped results = %q missing %q", grouped, want)
		}
	}
	if req.Messages[2].Parts[0].State != "" {
		t.Fatal("user parts must not carry assistant state")
	}
}

// A result immediately following a call with no intervening message is
// grouped; two separate runs are not.
func TestToolChatRequestGroupsOnlyConsecutiveResults(t *testing.T) {
	t.Parallel()
	hist := []openai.History{
		{Role: "user", Results: []openai.ToolResult{{CallID: "a", Content: "1"}}},
		{Role: "assistant", Texts: []string{"thinking"}},
		{Role: "user", Results: []openai.ToolResult{{CallID: "b", Content: "2"}}},
	}
	req := NewToolChatRequestHistory(hist, "", nil)
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d: %+v", len(req.Messages), req.Messages)
	}
}

// A tool result is named when the matching assistant call is in the history.
func TestToolChatRequestNamesResultFromCall(t *testing.T) {
	t.Parallel()
	hist := []openai.History{
		{Role: "assistant", Calls: []openai.ToolCall{{ID: "call_a", Function: openai.FunctionCall{Name: "get_weather"}}}},
		{Role: "user", Results: []openai.ToolResult{{CallID: "call_a", Content: "x"}}},
	}
	req := NewToolChatRequestHistory(hist, "", nil)
	if !contains(req.Messages[1].Parts[0].Text, `name="get_weather"`) {
		t.Fatalf("result = %q", req.Messages[1].Parts[0].Text)
	}
}

func marshalNormalised(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Ids are random; strip them so the comparison is about shape.
	b = regexp.MustCompile(`"[cm]-[0-9a-f]{12}"`).ReplaceAll(b, []byte(`"<id>"`))
	return string(b)
}

func contains(s, sub string) bool {
	return regexp.MustCompile(regexp.QuoteMeta(sub)).MatchString(s)
}
