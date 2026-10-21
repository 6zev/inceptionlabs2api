package openai

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeStrict(t *testing.T, body string) (*ChatRequest, *RequestError) {
	t.Helper()
	return Decode([]byte(body))
}

func TestDecodeAcceptsSupportedSubset(t *testing.T) {
	t.Parallel()
	req, re := decodeStrict(t, `{"model":"mercury-3","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	if re != nil {
		t.Fatalf("Decode: %v", re)
	}
	if req.Model != "mercury-3" || !req.Stream || req.ReasoningEffort != "high" {
		t.Fatalf("req = %+v", req)
	}
}

func TestDecodeDefaultReasoningEffort(t *testing.T) {
	t.Parallel()
	req, re := decodeStrict(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if re != nil {
		t.Fatalf("Decode: %v", re)
	}
	if req.ReasoningEffort != "medium" {
		t.Fatalf("default effort = %q, want medium", req.ReasoningEffort)
	}
}

// Untouched unknown fields are dropped silently: OpenAI SDKs send them on
// every ordinary call and the upstream assigns them no meaning.
func TestDecodeDropsUnknownParamsSilently(t *testing.T) {
	t.Parallel()
	req, re := decodeStrict(t, `{
		"model":"m",
		"messages":[{"role":"user","content":"hi"}],
		"temperature":0.2,"top_p":0.9,"stop":["x"],"n":2,
		"logprobs":true,"top_logprobs":3,"presence_penalty":0.1,
		"frequency_penalty":0.2,"seed":42,"user":"u-1",
		"response_format":{"type":"json_object"},"stream_options":{"include_usage":true},
		"logit_bias":{"50256":-100},"parallel_tool_calls":true,
		"some_future_field":{"nested":[1,2,3]}
	}`)
	if re != nil {
		t.Fatalf("Decode rejected a request with ignorable fields: %+v", re)
	}
	if req.Model != "m" || len(req.Messages) != 1 {
		t.Fatalf("req = %+v", req)
	}
}

// Tool calling is out of scope, but offering tools must not break the call:
// the fields are dropped and the request proceeds as plain chat.
func TestDecodeDropsToolFieldsAndProceeds(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"auto"}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":"f"}}}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"functions":[{"name":"f"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"function_call":"auto"}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[],"tool_choice":"none"}`,
	} {
		req, re := decodeStrict(t, body)
		if re != nil {
			t.Errorf("%s: rejected: %+v", body, re)
			continue
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
			t.Errorf("%s: messages = %+v", body, req.Messages)
		}
	}
}

// reasoning_effort is the upstream's enum. "minimal" has a documented mapping;
// anything else is passed through for the upstream to accept or reject.
func TestDecodeReasoningEffortPassThrough(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"minimal": "low",
		"low":     "low",
		"medium":  "medium",
		"high":    "high",
		"extreme": "extreme",
		"":        "medium",
	}
	for in, want := range cases {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + in + `"}`
		req, re := decodeStrict(t, body)
		if re != nil {
			t.Errorf("%q: rejected: %+v", in, re)
			continue
		}
		if req.ReasoningEffort != want {
			t.Errorf("%q: effort = %q, want %q", in, req.ReasoningEffort, want)
		}
	}
}

func TestDecodeRejectsMalformedBodies(t *testing.T) {
	t.Parallel()
	cases := []string{
		``,                            // empty body
		`{`,                           // truncated JSON
		`[]`,                          // not an object
		`null`,                        // not an object
		`{"model":"m","messages":[]}`, // no messages
		`{"messages":[{"role":"user","content":"hi"}]}`, // no model
		`{"model":"m"}`,                 // no messages key
		`{"model":"m","messages":"hi"}`, // wrong type
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":"yes"}`, // wrong type
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}{"x":1}`,         // trailing data
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}garbage`,         // trailing junk
	}
	for _, body := range cases {
		if _, re := decodeStrict(t, body); re == nil {
			t.Errorf("%q: expected a structural error", body)
		}
	}
}

// Trailing data must be reported as such, not as an opaque unmarshal error:
// all three JSON routes reject it for the same reason (SPEC §1.1 class 1).
func TestDecodeTrailingDataMessage(t *testing.T) {
	t.Parallel()
	_, re := decodeStrict(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}garbage`)
	if re == nil {
		t.Fatal("expected a structural error")
	}
	if !strings.Contains(re.Message, "unexpected data") {
		t.Fatalf("message = %q, want the trailing-data explanation", re.Message)
	}
}

func TestNormalizeRoles(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "developer", Content: "dev"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: []any{map[string]any{"type": "text", "text": "answer"}}},
	}
	out, total, re := NormalizeMessages(msgs)
	if re != nil {
		t.Fatalf("NormalizeMessages: %v", re)
	}
	want := []TextMessage{
		{Role: "system", Texts: []string{"sys"}},
		{Role: "system", Texts: []string{"dev"}},
		{Role: "user", Texts: []string{"hello"}},
		{Role: "assistant", Texts: []string{"answer"}},
	}
	if len(out) != len(want) {
		t.Fatalf("messages = %d, want %d", len(out), len(want))
	}
	for i := range want {
		if out[i].Role != want[i].Role || strings.Join(out[i].Texts, "|") != strings.Join(want[i].Texts, "|") {
			t.Errorf("message %d = %+v, want %+v", i, out[i], want[i])
		}
	}
	if total != len("sys")+len("dev")+len("hello")+len("answer") {
		t.Errorf("total = %d", total)
	}
}

// Media parts are not rejected: the caller's attachment is rendered as a short
// text placeholder so the model still sees that something was sent.
func TestNormalizeRendersMediaAsPlaceholder(t *testing.T) {
	t.Parallel()
	msgs := []Message{{Role: "user", Content: []any{
		map[string]any{"type": "text", "text": "describe"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
		map[string]any{"type": "input_audio"},
		map[string]any{"type": "file"},
		map[string]any{"type": "video_url"},
	}}}
	out, _, re := NormalizeMessages(msgs)
	if re != nil {
		t.Fatalf("media rejected: %+v", re)
	}
	got := strings.Join(out[0].Texts, "|")
	for _, want := range []string{"describe", "[image]", "[audio]", "[file]", "[video_url]"} {
		if !strings.Contains(got, want) {
			t.Errorf("texts = %q, want %q", got, want)
		}
	}
}

func TestNormalizeCoercesToolRolesAndDropsToolCalls(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "call", ToolCalls: json.RawMessage(`[{"id":"call_1"}]`)},
		{Role: "tool", Content: "tool result", ToolCallID: json.RawMessage(`"call_1"`)},
		{Role: "function", Content: "fn result"},
		{Role: "assistant", Content: "x", FunctionCall: json.RawMessage(`{"name":"f"}`)},
		{Role: "weird", Content: "unknown role"},
	}
	out, _, re := NormalizeMessages(msgs)
	if re != nil {
		t.Fatalf("tool history rejected: %+v", re)
	}
	wantRoles := []string{"user", "assistant", "user", "user", "assistant", "user"}
	for i, want := range wantRoles {
		if out[i].Role != want {
			t.Errorf("message %d role = %q, want %q", i, out[i].Role, want)
		}
	}
	if len(out[1].Texts) != 1 || out[1].Texts[0] != "call" {
		t.Errorf("assistant tool_calls message texts = %v", out[1].Texts)
	}
}

func TestNormalizeRejectsMessagesWithoutText(t *testing.T) {
	t.Parallel()
	cases := []Message{
		{Role: "user", Content: ""},
		{Role: "user", Content: []any{}},
		{Role: "assistant", Content: nil},
		{Role: "user", Content: 42},
		{Role: "user", Content: []any{map[string]any{"type": "text"}}},
		{Role: "user", Content: []any{map[string]any{"type": "text", "text": ""}}},
		{Role: "user", Content: []any{"bare"}},
	}
	for i, m := range cases {
		if _, _, re := NormalizeMessages([]Message{m}); re == nil {
			t.Errorf("case %d: expected a structural error", i)
		}
	}
}

// There is no client-side length guard: the upstream owns its context limit.
func TestNormalizeHasNoLengthGuard(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("x", 1_000_000)
	out, total, re := NormalizeMessages([]Message{{Role: "user", Content: huge}})
	if re != nil {
		t.Fatalf("long input rejected: %+v", re)
	}
	if total != len(huge) || len(out) != 1 {
		t.Fatalf("total = %d, out = %d", total, len(out))
	}
}

// A flood of tiny text parts is forwarded too: there is no invented part cap.
func TestNormalizeHasNoPartCap(t *testing.T) {
	t.Parallel()
	parts := make([]any, 0, 5000)
	for i := 0; i < 5000; i++ {
		parts = append(parts, map[string]any{"type": "text", "text": "x"})
	}
	if _, _, re := NormalizeMessages([]Message{{Role: "user", Content: parts}}); re != nil {
		t.Fatalf("part flood rejected: %+v", re)
	}
}

func TestErrorEnvelopeShape(t *testing.T) {
	t.Parallel()
	re := New(429, "rate_limit_error", "too_many", "model", "slow down")
	rec := httptest.NewRecorder()
	WriteError(rec, re)
	if rec.Code != 429 {
		t.Fatalf("status = %d", rec.Code)
	}
	var env map[string]map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]string{"message": "slow down", "type": "rate_limit_error", "code": "too_many", "param": "model"}
	for k, v := range want {
		if env["error"][k] != v {
			t.Errorf("error.%s = %q, want %q", k, env["error"][k], v)
		}
	}
}

func TestErrorEnvelopeOmitsEmptyParam(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	WriteError(rec, Invalid("bad"))
	var raw map[string]map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["error"]["param"]; ok {
		t.Error("param should be omitted when empty")
	}
}

func TestRequestErrorMessage(t *testing.T) {
	t.Parallel()
	if got := Invalid("boom").Error(); got != "boom" {
		t.Fatalf("Error() = %q", got)
	}
}
