package openai

import (
	"encoding/json"
	"net/http"
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

func TestDecodeMinimalMapsToLow(t *testing.T) {
	t.Parallel()
	req, re := decodeStrict(t, `{"model":"m","reasoning_effort":"minimal","messages":[{"role":"user","content":"hi"}]}`)
	if re != nil {
		t.Fatalf("Decode: %v", re)
	}
	if req.ReasoningEffort != "low" {
		t.Fatalf("effort = %q, want low", req.ReasoningEffort)
	}
}

func TestDecodeRejectsUnknownParamsNamingAll(t *testing.T) {
	t.Parallel()
	_, re := decodeStrict(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.2,"top_p":0.9,"stop":["x"],"n":2}`)
	if re == nil {
		t.Fatal("expected error")
	}
	if re.Status != http.StatusBadRequest || re.Code != CodeUnsupportedParam {
		t.Fatalf("err = %+v, want 400 unsupported_parameter", re)
	}
	for _, key := range []string{"n", "stop", "temperature", "top_p"} {
		if !strings.Contains(re.Message, `"`+key+`"`) {
			t.Errorf("message %q does not name %q", re.Message, key)
		}
	}
	if re.Param != "n" {
		t.Errorf("param = %q, want first sorted key n", re.Param)
	}
	if !strings.Contains(re.Message, `"messages"`) || !strings.Contains(re.Message, `"reasoning_effort"`) {
		t.Errorf("message %q should list the supported set", re.Message)
	}
}

func TestDecodeRejectsToolFields(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"auto"}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"functions":[]}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"function_call":"auto"}`,
	} {
		_, re := decodeStrict(t, body)
		if re == nil || re.Code != CodeToolUnsupported {
			t.Errorf("%s: err = %+v, want tool_calling_unsupported", body, re)
		}
	}
}

func TestDecodeRejectsMalformedBodies(t *testing.T) {
	t.Parallel()
	cases := []string{
		``,
		`{`,
		`[]`,
		`null`,
		`{"model":"m","messages":[]}`,
		`{"messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m"}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"maximum"}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":"yes"}`,
	}
	for _, body := range cases {
		if _, re := decodeStrict(t, body); re == nil {
			t.Errorf("%q: expected error", body)
		}
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
	out, total, re := NormalizeMessages(msgs, 0)
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

func TestNormalizeRejectsMedia(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{"image_url", "input_audio", "file", "video_url"} {
		msgs := []Message{{Role: "user", Content: []any{
			map[string]any{"type": "text", "text": "describe"},
			map[string]any{"type": typ},
		}}}
		_, _, re := NormalizeMessages(msgs, 0)
		if re == nil || re.Code != CodeContentUnsupported {
			t.Errorf("type %s: err = %+v, want content_part_unsupported", typ, re)
		}
		if !strings.Contains(re.Message, typ) {
			t.Errorf("type %s: message %q does not name the part", typ, re.Message)
		}
	}
}

func TestNormalizeRejectsToolMessages(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "call", ToolCalls: json.RawMessage(`[{"id":"call_1"}]`)},
	}
	_, _, re := NormalizeMessages(msgs, 0)
	if re == nil || re.Code != CodeToolUnsupported {
		t.Fatalf("err = %+v, want tool_calling_unsupported", re)
	}

	msgs = []Message{{Role: "tool", Content: "result", ToolCallID: "call_1"}}
	_, _, re = NormalizeMessages(msgs, 0)
	if re == nil || re.Code != CodeToolUnsupported {
		t.Fatalf("err = %+v, want tool_calling_unsupported", re)
	}

	// Empty tool_calls arrays are ordinary history, not a tool request.
	msgs = []Message{
		{Role: "assistant", Content: "done", ToolCalls: json.RawMessage(`[]`)},
		{Role: "assistant", Content: "done", ToolCalls: json.RawMessage(`null`)},
	}
	if _, _, re := NormalizeMessages(msgs, 0); re != nil {
		t.Fatalf("empty tool_calls should be accepted, got %+v", re)
	}

	msgs = []Message{{Role: "bogus", Content: "x"}}
	if _, _, re := NormalizeMessages(msgs, 0); re == nil || !strings.Contains(re.Message, "unsupported role") {
		t.Fatalf("err = %+v, want unsupported role", re)
	}
}

func TestNormalizeRejectsEmptyContent(t *testing.T) {
	t.Parallel()
	cases := []Message{
		{Role: "user", Content: ""},
		{Role: "user", Content: []any{}},
		{Role: "assistant", Content: nil},
		{Role: "user", Content: 42},
		{Role: "user", Content: []any{map[string]any{"type": "text"}}},
		{Role: "user", Content: []any{"bare"}},
	}
	for i, m := range cases {
		if _, _, re := NormalizeMessages([]Message{m}, 0); re == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestContextGuardCountsRunes(t *testing.T) {
	t.Parallel()
	// 10 CJK characters count as 10, not 30 bytes.
	msgs := []Message{{Role: "user", Content: strings.Repeat("你", 10)}}
	_, total, re := NormalizeMessages(msgs, 10)
	if re != nil {
		t.Fatalf("re = %v", re)
	}
	if total != 10 {
		t.Fatalf("total = %d, want 10", total)
	}
	_, _, re = NormalizeMessages(msgs, 9)
	if re == nil || re.Code != CodeContextLength || re.Status != 400 {
		t.Fatalf("err = %+v, want context_length_exceeded", re)
	}
	if _, _, re := NormalizeMessages(msgs, -1); re != nil {
		t.Fatalf("disabled guard rejected input: %v", re)
	}
}

func TestNormalizeSumsAcrossMessages(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		{Role: "system", Content: strings.Repeat("s", 5000)},
		{Role: "user", Content: strings.Repeat("u", 5000)},
	}
	_, total, re := NormalizeMessages(msgs, 9_000)
	if re == nil || re.Code != CodeContextLength {
		t.Fatalf("err = %+v, want context_length_exceeded", re)
	}
	if total != 10_000 {
		t.Fatalf("total = %d, want 10000", total)
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

func TestPrettyPrintedEmptyToolCallsAccepted(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`[]`, `[ ]`, "[\n  ]", `null`, `{}`, `{ }`} {
		msgs := []Message{{Role: "assistant", Content: "done", ToolCalls: json.RawMessage(raw)}}
		if _, _, re := NormalizeMessages(msgs, 0); re != nil {
			t.Errorf("tool_calls %q rejected: %+v", raw, re)
		}
	}
	for _, raw := range []string{`[{"id":"call_1"}]`, `{"name":"f"}`} {
		msgs := []Message{{Role: "assistant", Content: "done", ToolCalls: json.RawMessage(raw)}}
		if _, _, re := NormalizeMessages(msgs, 0); re == nil || re.Code != CodeToolUnsupported {
			t.Errorf("tool_calls %q: err = %+v, want tool_calling_unsupported", raw, re)
		}
	}
}

func TestPerMessageFunctionCallRejected(t *testing.T) {
	t.Parallel()
	msgs := []Message{{Role: "assistant", Content: "x", FunctionCall: json.RawMessage(`{"name":"f","arguments":"{}"}`)}}
	if _, _, re := NormalizeMessages(msgs, 0); re == nil || re.Code != CodeToolUnsupported {
		t.Fatalf("err = %+v, want function_call rejection", re)
	}
	msgs = []Message{{Role: "assistant", Content: "x", FunctionCall: json.RawMessage(`null`)}}
	if _, _, re := NormalizeMessages(msgs, 0); re != nil {
		t.Fatalf("null function_call rejected: %+v", re)
	}
}
