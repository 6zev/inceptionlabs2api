package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

// T19: structural tool errors are 400s in class 2; anything OpenAI accepts is
// accepted.
func TestDecodeToolsShape(t *testing.T) {
	t.Parallel()
	base := `"model":"m","messages":[{"role":"user","content":"hi"}]`

	good := []string{
		`"tools":[]`,
		`"tools":[{"type":"function","function":{"name":"f"}}]`,
		`"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}]`,
		// Missing "type" is tolerated: the only tool kind that exists is function.
		`"tools":[{"function":{"name":"f"}}]`,
		`"tools":[{"type":"function","function":{"name":"f","parameters":"not an object"}}]`,
		`"tool_choice":"auto"`,
		`"tool_choice":"none"`,
		`"tool_choice":"required"`,
		`"tool_choice":{"type":"function","function":{"name":"f"}}`,
		`"tool_choice":"bogus"`,
		`"parallel_tool_calls":false`,
	}
	for _, extra := range good {
		if _, re := decodeStrict(t, "{"+base+","+extra+"}"); re != nil {
			t.Errorf("%s: rejected: %+v", extra, re)
		}
	}

	bad := []string{
		`"tools":"nope"`,
		`"tools":{"name":"f"}`,
		`"tools":[{"type":"function"}]`,
		`"tools":[{"type":"function","function":{"name":""}}]`,
		`"tools":[{"function":{"name":123}}]`,
		`"tools":["f"]`,
	}
	for _, extra := range bad {
		_, re := decodeStrict(t, "{"+base+","+extra+"}")
		if re == nil {
			t.Errorf("%s: accepted, want 400", extra)
			continue
		}
		if re.Status != 400 || re.Code != CodeInvalidRequest {
			t.Errorf("%s: error = %+v", extra, re)
		}
	}
}

func TestDecodeToolChoiceNormalisation(t *testing.T) {
	t.Parallel()
	base := `"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]`

	cases := []struct {
		choice string
		want   ToolChoice
	}{
		{"", ToolChoice{Kind: ToolChoiceAuto}},
		{`"tool_choice":"auto"`, ToolChoice{Kind: ToolChoiceAuto}},
		{`"tool_choice":"none"`, ToolChoice{Kind: ToolChoiceNone}},
		{`"tool_choice":"required"`, ToolChoice{Kind: ToolChoiceRequired}},
		{`"tool_choice":"bogus"`, ToolChoice{Kind: ToolChoiceAuto}},
		{`"tool_choice":{"type":"function","function":{"name":"g"}}`, ToolChoice{Kind: ToolChoiceNamed, Name: "g"}},
	}
	for _, tc := range cases {
		body := "{" + base
		if tc.choice != "" {
			body += "," + tc.choice
		}
		body += "}"
		req, re := decodeStrict(t, body)
		if re != nil {
			t.Errorf("%s: %+v", body, re)
			continue
		}
		if req.ToolChoice != tc.want {
			t.Errorf("%s: choice = %+v, want %+v", body, req.ToolChoice, tc.want)
		}
	}
}

// A named choice with no function name falls back to auto rather than forcing
// the empty name.
func TestDecodeToolChoiceEmptyNamedFallsBackToAuto(t *testing.T) {
	t.Parallel()
	req, re := decodeStrict(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"function","function":{"name":""}}}`)
	if re != nil {
		t.Fatalf("Decode: %v", re)
	}
	if req.ToolChoice.Kind != ToolChoiceAuto {
		t.Fatalf("choice = %+v", req.ToolChoice)
	}
}

func TestDecodeLegacyFunctionFieldsAreDropped(t *testing.T) {
	t.Parallel()
	req, re := decodeStrict(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"functions":[{"name":"f"}],"function_call":"auto"}`)
	if re != nil {
		t.Fatalf("Decode: %v", re)
	}
	if len(req.Tools) != 0 || req.ToolChoice.Kind != ToolChoiceAuto {
		t.Fatalf("legacy fields leaked into tools: %+v", req)
	}
}

func TestDecodeAssistantToolCallsAndToolRole(t *testing.T) {
	t.Parallel()
	req, re := decodeStrict(t, `{"model":"m","messages":[
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_a","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}}]},
		{"role":"tool","tool_call_id":"call_a","content":"42"}]}`)
	if re != nil {
		t.Fatalf("Decode: %v", re)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if string(req.Messages[1].ToolCallID) != `"call_a"` {
		t.Fatalf("tool_call_id = %s", req.Messages[1].ToolCallID)
	}
	var calls []ToolCall
	if err := json.Unmarshal(req.Messages[0].ToolCalls, &calls); err != nil {
		t.Fatalf("tool_calls: %v", err)
	}
	if len(calls) != 1 || calls[0].ID != "call_a" || calls[0].Function.Name != "f" || calls[0].Function.Arguments != `{"x":1}` {
		t.Fatalf("calls = %+v", calls)
	}
}

// ToolCallID is nullable in the wild; it must not break decoding.
func TestDecodeToolMessageWithoutID(t *testing.T) {
	t.Parallel()
	if _, re := decodeStrict(t, `{"model":"m","messages":[{"role":"tool","content":"x"}]}`); re != nil {
		t.Fatalf("Decode: %v", re)
	}
}

// The replay encoder consumes this shape; pin the JSON field names.
func TestToolTypesJSONShape(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(ToolCall{ID: "call_a", Type: "function", Function: FunctionCall{Name: "f", Arguments: "{}"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"id":"call_a"`, `"type":"function"`, `"name":"f"`, `"arguments":"{}"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("missing %s in %s", key, b)
		}
	}
}
