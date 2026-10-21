package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/upstream"
)

// toolStream builds an upstream SSE body carrying one text part.
func toolStream(text string) string {
	return "data: {\"type\":\"text-start\",\"id\":\"text-0\"}\n\n" +
		"data: " + mustJSON(map[string]any{"type": "text-delta", "id": "text-0", "delta": text}) + "\n\n" +
		"data: {\"type\":\"text-end\",\"id\":\"text-0\"}\n\n" +
		"data: [DONE]\n\n"
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

const weatherToolJSON = `{"type":"function","function":{"name":"get_weather","description":"Get the weather.","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}`

func toolBody(stream bool, extra string) string {
	s := ""
	if stream {
		s = `"stream":true,`
	}
	return `{"model":"mercury-3",` + s + `"tools":[` + weatherToolJSON + `]` + extra + `,"messages":[{"role":"user","content":"weather in Paris?"}]}`
}

// T1: a request with no tools is byte-identical to the pre-v8 gateway. The
// upstream body carries no preamble and the SSE frames match the captured
// baseline apart from the random id and timestamp.
func TestNoToolsRequestIsUnchanged(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(hardStream), nil }}
	s := newTestServer(up)
	rec := postChat(t, s, `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	req := up.lastRequest()
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("upstream messages = %+v", req.Messages)
	}
	for _, m := range req.Messages {
		for _, p := range m.Parts {
			if strings.Contains(p.Text, "You have access to") || strings.Contains(p.Text, "<tools>") {
				t.Fatalf("preamble leaked into a tool-less request: %q", p.Text)
			}
		}
	}
	frames := sseFrames(t, rec.Body.String())
	if len(frames) < 3 || !strings.Contains(frames[len(frames)-2], `"finish_reason":"stop"`) {
		t.Fatalf("frames = %v", frames)
	}
	if strings.Contains(rec.Body.String(), "tool_calls") {
		t.Fatal("tool_calls in a tool-less response")
	}
}

// The golden capture from the running pre-change binary pins the exact frame
// sequence for a fixed prompt. The mock replays the same upstream events the
// live capture contained, and the gateway's frames must match the capture.
func TestNoToolsGoldenFrames(t *testing.T) {
	t.Parallel()
	golden, err := os.ReadFile("../../../testdata/no-tools-baseline.sse")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) {
		return streamOf("data: {\"type\":\"text-start\",\"id\":\"text-0\"}\n\n" +
			"data: {\"type\":\"text-delta\",\"id\":\"text-0\",\"delta\":\"bas\"}\n\n" +
			"data: {\"type\":\"text-delta\",\"id\":\"text-0\",\"delta\":\"eline-ok\"}\n\n" +
			"data: {\"type\":\"text-end\",\"id\":\"text-0\"}\n\n" +
			"data: [DONE]\n\n"), nil
	}}
	s := newTestServer(up)
	rec := postChat(t, s, `{"model":"mercury-3","stream":true,"reasoning_effort":"instant","messages":[{"role":"system","content":"Be terse."},{"role":"user","content":"Reply with exactly: baseline-ok"}]}`)
	got := normaliseFrames(rec.Body.String())
	want := normaliseFrames(string(golden))
	if got != want {
		t.Fatalf("frames differ from the pre-change capture:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

var (
	reChatID = regexp.MustCompile(`chatcmpl-[0-9a-f]{12}`)
	reTS     = regexp.MustCompile(`"created":\d+`)
)

func normaliseFrames(s string) string {
	s = reChatID.ReplaceAllString(s, "chatcmpl-<id>")
	return reTS.ReplaceAllString(s, `"created":<ts>`)
}

// T6: text and a call in one response: content frames, then the call chunk,
// then finish_reason tool_calls.
func TestStreamTextThenToolCall(t *testing.T) {
	t.Parallel()
	text := "Checking. <tool_call>{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Paris\"}}</tool_call>"
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(toolStream(text)), nil }}
	s := newTestServer(up)
	rec := postChat(t, s, toolBody(true, ""))
	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}

	frames := sseFrames(t, rec.Body.String())
	var content, calls int
	var finish string
	for _, f := range frames {
		if f == "[DONE]" {
			continue
		}
		var ch openai.Chunk
		if err := json.Unmarshal([]byte(f), &ch); err != nil {
			t.Fatalf("frame %s: %v", f, err)
		}
		d := ch.Choices[0].Delta
		if d.Content != "" {
			content++
			if strings.Contains(d.Content, "<tool_call>") {
				t.Fatalf("marker leaked as content: %q", d.Content)
			}
		}
		if len(d.ToolCalls) > 0 {
			calls++
			tc := d.ToolCalls[0]
			if tc.Index != 0 || !strings.HasPrefix(tc.ID, "call_") || tc.Type != "function" ||
				tc.Function.Name != "get_weather" || !json.Valid([]byte(tc.Function.Arguments)) {
				t.Fatalf("tool delta = %+v", tc)
			}
		}
		if ch.Choices[0].FinishReason != nil {
			finish = *ch.Choices[0].FinishReason
		}
	}
	if content != 1 || calls != 1 || finish != "tool_calls" {
		t.Fatalf("content=%d calls=%d finish=%q frames=%v", content, calls, finish, frames)
	}
	if !strings.Contains(rec.Body.String(), `"content":"Checking. "`) {
		t.Fatalf("content frame text wrong: %s", rec.Body.String())
	}
}

// T7: parallel calls carry distinct indexes and ids.
func TestStreamParallelCalls(t *testing.T) {
	t.Parallel()
	text := `<tool_call>{"name":"get_weather","arguments":{"city":"Paris"}}</tool_call>` +
		`<tool_call>{"name":"get_weather","arguments":{"city":"Lyon"}}</tool_call>`
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(toolStream(text)), nil }}
	s := newTestServer(up)
	rec := postChat(t, s, toolBody(true, ""))
	frames := sseFrames(t, rec.Body.String())
	var got []openai.ToolCallDelta
	for _, f := range frames {
		var ch openai.Chunk
		if err := json.Unmarshal([]byte(f), &ch); err != nil {
			continue
		}
		got = append(got, ch.Choices[0].Delta.ToolCalls...)
	}
	if len(got) != 2 {
		t.Fatalf("calls = %+v", got)
	}
	if got[0].Index != 0 || got[1].Index != 1 {
		t.Fatalf("indexes = %d,%d", got[0].Index, got[1].Index)
	}
	if got[0].ID == got[1].ID {
		t.Fatalf("ids collide: %s", got[0].ID)
	}
	if got[0].Function.Arguments != `{"city":"Paris"}` || got[1].Function.Arguments != `{"city":"Lyon"}` {
		t.Fatalf("arguments = %s / %s", got[0].Function.Arguments, got[1].Function.Arguments)
	}
}

// T2: prose only, no call, no marker leakage.
func TestStreamProseOnly(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(toolStream("It is sunny.")), nil }}
	s := newTestServer(up)
	rec := postChat(t, s, toolBody(true, ""))
	if strings.Contains(rec.Body.String(), "tool_calls") {
		t.Fatalf("unexpected call: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"content":"It is sunny."`) {
		t.Fatalf("content missing: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Fatalf("finish missing: %s", rec.Body)
	}
}

// T11: tool_choice required but the model answers prose: honest stop.
func TestRequiredButProse(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(toolStream("I would rather not.")), nil }}
	s := newTestServer(up)
	rec := postChat(t, s, toolBody(false, `,"tool_choice":"required"`))
	var c openai.Completion
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	ch := c.Choices[0]
	if ch.FinishReason != "stop" || ch.Message.Content != "I would rather not." || len(ch.Message.ToolCalls) != 0 {
		t.Fatalf("choice = %+v", ch)
	}
	if !strings.Contains(up.lastRequest().Messages[0].Parts[0].Text, "MUST use") {
		t.Fatal("required instruction missing from the preamble")
	}
}

// T12: named choice lists only that tool.
func TestNamedChoiceTrimsManifest(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(toolStream("ok")), nil }}
	s := newTestServer(up)
	body := `{"model":"mercury-3","tools":[` + weatherToolJSON + `,{"type":"function","function":{"name":"other"}}],"tool_choice":{"type":"function","function":{"name":"other"}},"messages":[{"role":"user","content":"q"}]}`
	postChat(t, s, body)
	preamble := up.lastRequest().Messages[0].Parts[0].Text
	if strings.Contains(preamble, "get_weather") || !strings.Contains(preamble, `"name":"other"`) {
		t.Fatalf("preamble = %s", preamble)
	}
}

// T13: tool_choice none takes the legacy path even with tools present.
func TestToolChoiceNoneTakesLegacyPath(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(toolStream("plain")), nil }}
	s := newTestServer(up)
	rec := postChat(t, s, toolBody(true, `,"tool_choice":"none"`))
	req := up.lastRequest()
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if strings.Contains(rec.Body.String(), "You have access to") {
		t.Fatal("preamble injected despite tool_choice none")
	}
	// A <tool_call> in the output must also not be parsed.
	up.fn = func(int) (*upstream.Stream, error) {
		return streamOf(toolStream(`x <tool_call>{"name":"get_weather","arguments":{}}</tool_call>`)), nil
	}
	rec = postChat(t, s, toolBody(true, `,"tool_choice":"none"`))
	if !strings.Contains(rec.Body.String(), "tool_call") || strings.Contains(rec.Body.String(), "tool_calls") {
		t.Fatalf("tool_choice none parsed a call: %s", rec.Body)
	}
}

// T20: citations ride the finish frame next to a call.
func TestToolCallKeepsCitations(t *testing.T) {
	t.Parallel()
	body := "data: {\"type\":\"source-url\",\"sourceId\":\"https://example.com/a\",\"url\":\"https://example.com/a\",\"title\":\"A\"}\n\n" +
		toolStream(`<tool_call>{"name":"get_weather","arguments":{}}</tool_call>`)
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(body), nil }}
	s := newTestServer(up)
	rec := postChat(t, s, toolBody(true, ""))
	if !strings.Contains(rec.Body.String(), `"url":"https://example.com/a"`) {
		t.Fatalf("citation lost: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish lost: %s", rec.Body)
	}
}

// Non-streaming shape: content is "" when the turn is only a call.
func TestNonStreamToolCall(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) {
		return streamOf(toolStream(`<tool_call>{"name":"get_weather","arguments":{"city":"Paris"}}</tool_call>`)), nil
	}}
	s := newTestServer(up)
	rec := postChat(t, s, toolBody(false, ""))
	var c openai.Completion
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ch := c.Choices[0]
	if ch.FinishReason != "tool_calls" || ch.Message.Content != "" || len(ch.Message.ToolCalls) != 1 {
		t.Fatalf("choice = %+v", ch)
	}
	if ch.Message.ToolCalls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("arguments = %q", ch.Message.ToolCalls[0].Function.Arguments)
	}
}

// T14 end to end: the replayed history reaches the upstream as one assistant
// turn of blocks and one grouped user turn of results.
func TestRoundsTripReachesUpstream(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(toolStream("21 C")), nil }}
	s := newTestServer(up)
	body := `{"model":"mercury-3","tools":[` + weatherToolJSON + `],"messages":[
		{"role":"user","content":"weather in Paris?"},
		{"role":"assistant","content":"Let me check.","tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},{"id":"call_b","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Lyon\"}"}}]},
		{"role":"tool","tool_call_id":"call_a","content":"{\"tempC\":21}"},
		{"role":"tool","tool_call_id":"call_b","content":"{\"tempC\":18}"}]}`
	rec := postChat(t, s, body)
	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	msgs := up.lastRequest().Messages
	// preamble, user, assistant, grouped results
	if len(msgs) != 4 {
		t.Fatalf("upstream messages = %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "system" {
		t.Fatalf("first role = %q", msgs[0].Role)
	}
	asst := msgs[2].Parts[0].Text
	if strings.Count(asst, "<tool_call>") != 2 || !strings.Contains(asst, `"id":"call_a"`) {
		t.Fatalf("assistant replay = %q", asst)
	}
	grouped := msgs[3]
	if grouped.Role != "user" || strings.Count(grouped.Parts[0].Text, "<tool_response") != 2 {
		t.Fatalf("grouped = %+v", grouped)
	}
	if !strings.Contains(grouped.Parts[0].Text, `name="get_weather"`) {
		t.Fatalf("results not named: %q", grouped.Parts[0].Text)
	}
}

// T21/T22: above the threshold, the first upstream call has no parameter
// schemas; a loader call triggers one re-ask that does, and the client never
// sees the loader.
func TestTieredLoaderResolvesInternally(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var prompts []string
	call := 0
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) {
		mu.Lock()
		defer mu.Unlock()
		call++
		first := call == 1
		if first {
			return streamOf(toolStream(`<tool_call>{"name":"tool_schema","arguments":{"names":["tool_3"]}}</tool_call>`)), nil
		}
		return streamOf(toolStream(`<tool_call>{"name":"tool_3","arguments":{"x":"v"}}</tool_call>`)), nil
	}}
	upWithCapture := &capturingChatter{inner: up, prompts: &prompts}
	s := newTestServer(upWithCapture)
	rec := postChat(t, s, tieredBody(true))
	if rec.Code != 200 {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body)
	}
	mu.Lock()
	n := call
	mu.Unlock()
	if n != 2 {
		t.Fatalf("upstream rounds = %d, want 2", n)
	}
	if len(prompts) != 2 {
		t.Fatalf("prompts = %d", len(prompts))
	}
	if !strings.Contains(prompts[0], toolSchemaLoader) || strings.Contains(prompts[0], `"parameters"`) {
		t.Fatalf("tier-1 prompt wrong:\n%s", prompts[0])
	}
	if !strings.Contains(prompts[1], `"name":"tool_3","description":"operation 3","parameters":{"type":"object"`) {
		t.Fatalf("expanded prompt lacks tool_3 schema:\n%s", prompts[1])
	}
	out := rec.Body.String()
	if strings.Contains(out, "tool_schema") {
		t.Fatalf("loader leaked to the client: %s", out)
	}
	if !strings.Contains(out, `"name":"tool_3"`) || !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("final call missing: %s", out)
	}
}

// T23: a loader call on both rounds stops resolving and never reaches the
// client.
func TestTieredLoaderTwiceStops(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	call := 0
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) {
		mu.Lock()
		defer mu.Unlock()
		call++
		return streamOf(toolStream(`<tool_call>{"name":"tool_schema","arguments":{"query":"tool"}}</tool_call>`)), nil
	}}
	s := newTestServer(up)
	rec := postChat(t, s, tieredBody(false))
	mu.Lock()
	n := call
	mu.Unlock()
	if n != 2 {
		t.Fatalf("rounds = %d, want 2 (initial + one expansion)", n)
	}
	var c openai.Completion
	if err := json.Unmarshal(rec.Body.Bytes(), &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(c.Choices[0].Message.ToolCalls) != 0 || c.Choices[0].FinishReason != "stop" {
		t.Fatalf("choice = %+v", c.Choices[0])
	}
	if strings.Contains(rec.Body.String(), "tool_schema") {
		t.Fatalf("loader leaked: %s", rec.Body)
	}
}

const toolSchemaLoader = "tool_schema"

// capturingChatter records every preamble handed to the upstream.
type capturingChatter struct {
	inner   Chatter
	mu      sync.Mutex
	prompts *[]string
}

func (c *capturingChatter) Chat(ctx context.Context, req *upstream.ChatRequest, idle time.Duration, onMalformed func(string)) (*upstream.Stream, error) {
	c.mu.Lock()
	for _, m := range req.Messages {
		if m.Role == "system" && len(m.Parts) > 0 {
			*c.prompts = append(*c.prompts, m.Parts[0].Text)
			break
		}
	}
	c.mu.Unlock()
	return c.inner.Chat(ctx, req, idle, onMalformed)
}

func tieredBody(stream bool) string {
	var tools []string
	for i := 0; i < 31; i++ {
		tools = append(tools, fmt.Sprintf(`{"type":"function","function":{"name":"tool_%d","description":"operation %d","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}}`, i, i))
	}
	s := ""
	if stream {
		s = `"stream":true,`
	}
	return `{"model":"mercury-3",` + s + `"tools":[` + strings.Join(tools, ",") + `],"messages":[{"role":"user","content":"use a tool"}]}`
}
