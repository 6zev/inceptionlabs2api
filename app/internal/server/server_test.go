package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"inceptionlabs2api/internal/clear"
	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/pool"
	"inceptionlabs2api/internal/upstream"
)

// fakeChatter scripts upstream streams per call.
type fakeChatter struct {
	mu    sync.Mutex
	calls int
	reqs  []*upstream.ChatRequest
	fn    func(call int) (*upstream.Stream, error)
}

func (f *fakeChatter) Chat(ctx context.Context, req *upstream.ChatRequest, idle time.Duration, onMalformed func(string)) (*upstream.Stream, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.reqs = append(f.reqs, req)
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		return nil, errors.New("no scripted stream")
	}
	return fn(call)
}

func (f *fakeChatter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeChatter) lastRequest() *upstream.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return nil
	}
	return f.reqs[len(f.reqs)-1]
}

// streamOf builds a Stream over a fixed SSE body.
func streamOf(lines string) *upstream.Stream {
	body := io.NopCloser(strings.NewReader(lines))
	return &upstream.Stream{Body: body, Reader: upstream.NewReader(body, time.Second, nil)}
}

// blockingStream emits the first chunk, then blocks until release is closed
// or the read context is cancelled.
func blockingStream(first, rest string, release <-chan struct{}) *upstream.Stream {
	r, w := io.Pipe()
	go func() {
		io.WriteString(w, first)
		select {
		case <-release:
			io.WriteString(w, rest)
			w.Close()
		case <-time.After(10 * time.Second):
			w.CloseWithError(errors.New("test deadline"))
		}
	}()
	return &upstream.Stream{Body: r, Reader: upstream.NewReader(r, time.Second, nil)}
}

type fakePool struct{ stats pool.Stats }

func (f fakePool) Stats() pool.Stats { return f.stats }

func newTestServer(up Chatter) *Server {
	s := New(Config{
		APIKey:      "",
		Model:       "mercury-3",
		Models:      []string{"mercury-3", "mercury-2"},
		IdleTimeout: 60 * time.Second,
		Logf:        func(string, ...any) {},
	}, up, fakePool{stats: pool.Stats{Size: 2, Min: 2, Max: 8}})
	s.SetClock(func() time.Time { return time.Unix(1_792_553_057, 0) })
	return s
}

// fakeSearcher scripts upstream search runs per call.
type fakeSearcher struct {
	mu    sync.Mutex
	calls int
	reqs  []*upstream.SearchRequest
	fn    func(call int) (*upstream.SearchResult, error)
	cfg   *upstream.SearchConfigResponse
	cfgFn func(ctx context.Context) (*upstream.SearchConfigResponse, error)
}

func (f *fakeSearcher) Search(ctx context.Context, req *upstream.SearchRequest, idle time.Duration) (*upstream.SearchResult, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.reqs = append(f.reqs, req)
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		return &upstream.SearchResult{}, nil
	}
	return fn(call)
}

func (f *fakeSearcher) SearchConfig(ctx context.Context) (*upstream.SearchConfigResponse, error) {
	if f.cfgFn != nil {
		return f.cfgFn(ctx)
	}
	if f.cfg == nil {
		return &upstream.SearchConfigResponse{RetrievalAvailable: true}, nil
	}
	return f.cfg, nil
}

func (f *fakeSearcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeSearcher) lastRequest() *upstream.SearchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return nil
	}
	return f.reqs[len(f.reqs)-1]
}

func postChat(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// sseFrames splits `data: ...` frames and decodes them.
func sseFrames(t *testing.T, body string) []string {
	t.Helper()
	var frames []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			frames = append(frames, strings.TrimPrefix(line, "data: "))
		}
	}
	return frames
}

func decodeChunk(t *testing.T, frame string) openai.Chunk {
	t.Helper()
	var c openai.Chunk
	if err := json.Unmarshal([]byte(frame), &c); err != nil {
		t.Fatalf("decode chunk %q: %v", frame, err)
	}
	return c
}

const hardStream = `data: {"type":"reasoning-start","id":"reasoning-0"}

data: {"type":"reasoning-delta","id":"reasoning-0","delta":"Analyzing the claim.\n"}

data: {"type":"reasoning-end","id":"reasoning-0"}

data: {"type":"text-start","id":"text-0"}

data: {"type":"text-delta","id":"text-0","delta":"The answer "}

data: {"type":"text-delta","id":"text-0","delta":"is 42."}

data: {"type":"text-end","id":"text-0"}

data: [DONE]

`

func TestStreamFraming(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(hardStream), nil }}
	s := newTestServer(up)
	rec := postChat(t, s, `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatalf("missing X-Accel-Buffering: no")
	}

	frames := sseFrames(t, rec.Body.String())
	if len(frames) < 2 {
		t.Fatalf("frames = %v", frames)
	}
	if frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("last frame = %q, want [DONE]", frames[len(frames)-1])
	}

	role := decodeChunk(t, frames[0])
	if role.Object != "chat.completion.chunk" || role.Model != "mercury-3" || role.Created != 1_792_553_057 {
		t.Fatalf("role chunk = %+v", role)
	}
	if !strings.HasPrefix(role.ID, "chatcmpl-") {
		t.Fatalf("id = %q", role.ID)
	}
	if role.Choices[0].Delta.Role != "assistant" || role.Choices[0].FinishReason != nil {
		t.Fatalf("role delta = %+v", role.Choices[0])
	}

	var reasoning, content []string
	var finish *string
	for _, f := range frames[1 : len(frames)-1] {
		c := decodeChunk(t, f)
		if c.ID != role.ID {
			t.Errorf("frame id %q != %q", c.ID, role.ID)
		}
		if c.Choices[0].Delta.ReasoningContent != "" {
			reasoning = append(reasoning, c.Choices[0].Delta.ReasoningContent)
		}
		if c.Choices[0].Delta.Content != "" {
			content = append(content, c.Choices[0].Delta.Content)
		}
		if c.Choices[0].FinishReason != nil {
			finish = c.Choices[0].FinishReason
		}
	}
	if strings.Join(reasoning, "") != "Analyzing the claim.\n" {
		t.Fatalf("reasoning = %q", reasoning)
	}
	if strings.Join(content, "") != "The answer is 42." {
		t.Fatalf("content = %q", content)
	}
	// Reasoning must precede content (first 17.5 s TTFT defect).
	if len(reasoning) == 0 || len(content) == 0 {
		t.Fatal("missing reasoning or content deltas")
	}
	firstContent := -1
	firstReasoning := -1
	for i, f := range frames[1 : len(frames)-1] {
		c := decodeChunk(t, f)
		if firstReasoning < 0 && c.Choices[0].Delta.ReasoningContent != "" {
			firstReasoning = i
		}
		if firstContent < 0 && c.Choices[0].Delta.Content != "" {
			firstContent = i
		}
	}
	if firstReasoning > firstContent {
		t.Fatalf("reasoning frame %d after content frame %d", firstReasoning, firstContent)
	}
	if finish == nil || *finish != "stop" {
		t.Fatalf("finish_reason = %v, want stop", finish)
	}
	if strings.Contains(rec.Body.String(), "usage") {
		t.Fatal("stream must not fabricate usage")
	}
}

func TestStreamMultiBlockAndSnapshot(t *testing.T) {
	t.Parallel()
	body := `data: {"type":"reasoning-start","id":"reasoning-0"}

data: {"type":"reasoning-delta","id":"reasoning-0","delta":"first thought"}

data: {"type":"reasoning-end","id":"reasoning-0"}

data: {"type":"text-start","id":"text-0","text":"snapshot block"}

data: {"type":"text-delta","id":"text-0","delta":" plus delta"}

data: {"type":"text-end","id":"text-0"}

data: {"type":"source-url","sourceId":"s1","url":"https://example.com","title":"Example"}

data: {"type":"reasoning-start","id":"reasoning-1"}

data: {"type":"reasoning-delta","id":"reasoning-1","delta":"second thought"}

data: {"type":"reasoning-end","id":"reasoning-1"}

data: {"type":"text-start","id":"text-1"}

data: {"type":"text-delta","id":"text-1","delta":" second answer"}

data: {"type":"text-end","id":"text-1"}

data: [DONE]

`
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(body), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	frames := sseFrames(t, rec.Body.String())
	var reasoning, content strings.Builder
	for _, f := range frames {
		if f == "[DONE]" {
			continue
		}
		c := decodeChunk(t, f)
		reasoning.WriteString(c.Choices[0].Delta.ReasoningContent)
		content.WriteString(c.Choices[0].Delta.Content)
	}
	if got := reasoning.String(); got != "first thoughtsecond thought" {
		t.Fatalf("reasoning = %q, want both blocks", got)
	}
	if got := content.String(); got != "snapshot block plus delta second answer" {
		t.Fatalf("content = %q", got)
	}
	// The source becomes a citation annotation on the finish frame (§2.4).
	if !strings.Contains(rec.Body.String(), `"type":"url_citation"`) || !strings.Contains(rec.Body.String(), "example.com") {
		t.Fatal("source-url should become a url_citation annotation")
	}
}

func TestStreamMidStreamFailureEmitsErrorAndDone(t *testing.T) {
	t.Parallel()
	body := `data: {"type":"text-delta","id":"text-0","delta":"partial"}

`
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(body), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("headers should already be committed, status = %d", rec.Code)
	}
	out := rec.Body.String()
	frames := sseFrames(t, out)
	if frames[len(frames)-1] != "[DONE]" {
		t.Fatalf("stream must end with [DONE], frames = %v", frames)
	}
	var errFrame map[string]map[string]any
	found := false
	for _, f := range frames[:len(frames)-1] {
		if strings.Contains(f, `"error"`) {
			if err := json.Unmarshal([]byte(f), &errFrame); err != nil {
				t.Fatalf("error frame: %v", err)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("no error frame in %s", out)
	}
	if errFrame["error"]["code"] != openai.CodeUpstreamStream {
		t.Fatalf("error code = %v", errFrame["error"]["code"])
	}
	if !strings.Contains(out, `"delta":{"content":"partial"}`) {
		t.Fatal("already-emitted content should survive")
	}
}

func TestStreamIdleTimeoutEmitsErrorAndDone(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	first := "data: {\"type\":\"text-delta\",\"id\":\"text-0\",\"delta\":\"a\"}\n\n"
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) {
		body := blockingStream(first, first, release)
		body.Reader = upstream.NewReader(body.Body, 50*time.Millisecond, nil)
		return body, nil
	}}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	out := rec.Body.String()
	if !strings.Contains(out, openai.CodeUpstreamIdle) {
		t.Fatalf("missing idle error frame: %s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "[DONE]") {
		t.Fatalf("stream must end with [DONE]: %s", out)
	}
}

func TestNonStreamAggregates(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(hardStream), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["object"] != "chat.completion" || got["model"] != "mercury-3" {
		t.Fatalf("envelope = %v", got)
	}
	if _, ok := got["usage"]; ok {
		t.Fatal("usage must be omitted, not fabricated")
	}
	choice := got["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "The answer is 42." {
		t.Fatalf("content = %v", msg["content"])
	}
	if msg["reasoning_content"] != "Analyzing the claim.\n" {
		t.Fatalf("reasoning_content = %v", msg["reasoning_content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	if _, ok := choice["logprobs"]; !ok {
		t.Fatal("logprobs key should be present (null)")
	}
}

func TestNonStreamReasoningOnlyHasEmptyContent(t *testing.T) {
	t.Parallel()
	body := `data: {"type":"reasoning-delta","id":"reasoning-0","delta":"thinking"}

data: [DONE]

`
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(body), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got openai.Completion
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Choices[0].Message.Content != "" {
		t.Fatalf("content = %q, want empty string", got.Choices[0].Message.Content)
	}
	if got.Choices[0].Message.ReasoningContent != "thinking" {
		t.Fatalf("reasoning = %q", got.Choices[0].Message.ReasoningContent)
	}
}

func TestMalformedSSELineIsSkipped(t *testing.T) {
	t.Parallel()
	body := `data: {not json}

data: {"type":"text-delta","id":"text-0","delta":"ok"}

data: [DONE]

`
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(body), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"content":"ok"`) {
		t.Fatalf("stream aborted on a malformed line: %d %s", rec.Code, rec.Body)
	}
}

func TestUpstreamUnavailableBeforeHeaders(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) {
		return nil, errors.Join(upstream.ErrUnavailable, errors.New("429 challenge"))
	}}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q", rec.Header().Get("Content-Type"))
	}
	var env openai.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != openai.CodeUpstreamUnavailable || env.Error.Type != "server_error" {
		t.Fatalf("envelope = %+v", env)
	}
}

// The upstream's own error prose is translated into the envelope, but an HTML
// challenge page is never echoed.
func TestUpstreamStatusErrorTranslatesProse(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) {
		return nil, &upstream.StatusError{Status: 400, Body: `{"error":{"message":"context window exceeded"}}`}
	}}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var env openai.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if env.Error.Message != "context window exceeded" {
		t.Fatalf("message = %q, want the upstream message", env.Error.Message)
	}
}

func TestUpstreamStatusErrorDoesNotLeakBody(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) {
		return nil, &upstream.StatusError{Status: 500, Body: "<html>secret challenge token</html>"}
	}}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "<html>") {
		t.Fatalf("upstream body leaked: %s", rec.Body)
	}
}

func TestAuth(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(hardStream), nil }}
	s := newTestServer(up)
	s.cfg.APIKey = "sekret"

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"mercury-3","messages":[{"role":"user","content":"q"}]}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing key: status = %d, want 401", rec.Code)
	}
	var env openai.ErrorResponse
	json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != openai.CodeInvalidAPIKey {
		t.Fatalf("code = %q", env.Error.Code)
	}

	for name, header := range map[string][2]string{
		"bearer":  {"Authorization", "Bearer sekret"},
		"api-key": {"X-Api-Key", "sekret"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set(header[0], header[1])
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", name, rec.Code)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: status = %d, want 401", rec.Code)
	}

	health := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, health)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz behind auth: %d", rec.Code)
	}
}

func TestMethodAndModelErrors(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{}
	s := newTestServer(up)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET chat: status = %d, want 405", rec.Code)
	}

	rec = postChat(t, s, `{"model":"nope","messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model: status = %d, want 404", rec.Code)
	}
	var env openai.ErrorResponse
	json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Code != openai.CodeModelNotFound {
		t.Fatalf("code = %q", env.Error.Code)
	}
	if up.count() != 0 {
		t.Fatal("unknown model must not reach upstream")
	}
}

// Only structural problems are rejected. Everything else is forwarded with
// the upstream left to judge it (SPEC §1.1).
func TestStructuralValidationNeverReachesUpstream(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"malformed", `{`, 400, openai.CodeInvalidRequest},
		{"not an object", `[]`, 400, openai.CodeInvalidRequest},
		{"empty messages", `{"model":"mercury-3","messages":[]}`, 400, openai.CodeInvalidRequest},
		{"no model", `{"messages":[{"role":"user","content":"q"}]}`, 400, openai.CodeInvalidRequest},
		{"no text", `{"model":"mercury-3","messages":[{"role":"user","content":""}]}`, 400, openai.CodeInvalidRequest},
		{"messages wrong type", `{"model":"mercury-3","messages":"q"}`, 400, openai.CodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeChatter{}
			rec := postChat(t, newTestServer(up), tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.status, rec.Body)
			}
			var env openai.ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode %s: %v", rec.Body, err)
			}
			if env.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q", env.Error.Code, tc.code)
			}
			if up.count() != 0 {
				t.Fatal("invalid request reached upstream")
			}
		})
	}
}

// The former 400s for unknown params, tools, media, tool roles, and unknown
// effort modes are gone: those requests now reach the upstream as plain chat.
func TestPresumptuousRejectionsAreGone(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{"unknown param", `{"model":"mercury-3","messages":[{"role":"user","content":"q"}],"temperature":0.5}`},
		{"tools", `{"model":"mercury-3","messages":[{"role":"user","content":"q"}],"tools":[{"type":"function","function":{"name":"f"}}]}`},
		{"tool_choice", `{"model":"mercury-3","messages":[{"role":"user","content":"q"}],"tool_choice":"auto"}`},
		{"image", `{"model":"mercury-3","messages":[{"role":"user","content":[{"type":"text","text":"see"},{"type":"image_url","image_url":{"url":"x"}}]}]}`},
		{"tool message", `{"model":"mercury-3","messages":[{"role":"tool","content":"x","tool_call_id":"c"}]}`},
		{"bad effort", `{"model":"mercury-3","messages":[{"role":"user","content":"q"}],"reasoning_effort":"max"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(hardStream), nil }}
			rec := postChat(t, newTestServer(up), tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
			}
			if up.count() != 1 {
				t.Fatalf("upstream calls = %d, want 1", up.count())
			}
		})
	}
}

// There is no client-side context guard: oversized input is forwarded and the
// upstream truncates at its own limit.
func TestLongInputPassesThrough(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(hardStream), nil }}
	big := strings.Repeat("x", 250_001)
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","messages":[{"role":"user","content":"`+big+`"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	got := up.lastRequest().Messages[0].Parts[0].Text
	if len(got) != len(big) {
		t.Fatalf("forwarded text = %d chars, want %d", len(got), len(big))
	}
}

func TestReasoningEffortForwarded(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(hardStream), nil }}
	postChat(t, newTestServer(up), `{"model":"mercury-3","reasoning_effort":"high","messages":[{"role":"user","content":"q"}]}`)
	if got := up.lastRequest().ReasoningEffort; got != "high" {
		t.Fatalf("effort = %q", got)
	}
}

func TestModelsEndpoint(t *testing.T) {
	t.Parallel()
	s := newTestServer(&fakeChatter{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var list openai.ModelList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Object != "list" || len(list.Data) != 2 {
		t.Fatalf("list = %+v", list)
	}
	for _, m := range list.Data {
		if m.Object != "model" || m.OwnedBy != "inceptionlabs" || m.Created != 1_792_553_057 {
			t.Errorf("model = %+v", m)
		}
	}
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	s := newTestServer(&fakeChatter{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["ok"] != true {
		t.Fatalf("ok = %v", body["ok"])
	}
	p := body["pool"].(map[string]any)
	if p["size"] != float64(2) || p["min"] != float64(2) || p["max"] != float64(8) {
		t.Fatalf("pool = %v", p)
	}
	counters := body["counters"].(map[string]any)
	for _, key := range []string{"requests", "streams", "errors", "upstream_errors"} {
		if _, ok := counters[key]; !ok {
			t.Errorf("counters missing %q: %v", key, counters)
		}
	}
}

func TestHealthzHead(t *testing.T) {
	t.Parallel()
	s := newTestServer(&fakeChatter{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d", rec.Code)
	}
}

func TestCountersAccumulate(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(hardStream), nil }}
	s := newTestServer(up)
	postChat(t, s, `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	postChat(t, s, `{"model":"mercury-3","messages":[{"role":"user","content":"q"}]}`)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body struct {
		Counters Counters `json:"counters"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Counters.Requests != 2 || body.Counters.Streams != 1 {
		t.Fatalf("counters = %+v", body.Counters)
	}
}

func TestCORSPreflightWithAPIKey(t *testing.T) {
	t.Parallel()
	s := newTestServer(&fakeChatter{})
	s.cfg.APIKey = "sekret"
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight with a key configured: status = %d, want 204 (preflights carry no credentials)", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("missing allow-origin on preflight")
	}
}

func TestCORSPreflight(t *testing.T) {
	t.Parallel()
	s := newTestServer(&fakeChatter{})
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want 204", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("missing allow-origin")
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("allow-headers = %q", rec.Header().Get("Access-Control-Allow-Headers"))
	}
}

func TestCORSHeadersOnResponse(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(hardStream), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","messages":[{"role":"user","content":"q"}]}`)
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("CORS header missing on a normal response")
	}
}

func TestTrailingJSONRejected(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","messages":[{"role":"user","content":"q"}]}{"x":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	var env openai.ErrorResponse
	json.Unmarshal(rec.Body.Bytes(), &env)
	if !strings.Contains(env.Error.Message, "unexpected data") {
		t.Fatalf("message = %q", env.Error.Message)
	}
}

func TestEmptyToolCallsAccepted(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(hardStream), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","messages":[
		{"role":"user","content":"q"},
		{"role":"assistant","content":"a","tool_calls":[]},
		{"role":"user","content":"q2"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if got := len(up.lastRequest().Messages); got != 3 {
		t.Fatalf("messages = %d, want 3", got)
	}
}

func TestEarlyFailureErrorCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"idle", upstream.ErrIdleTimeout, http.StatusGatewayTimeout, openai.CodeUpstreamIdle},
		{"deadline", context.DeadlineExceeded, http.StatusGatewayTimeout, openai.CodeUpstreamIdle},
		{"truncated", upstream.ErrStreamTruncated, http.StatusBadGateway, openai.CodeUpstreamStream},
		{"auth", upstream.ErrAuthRejected, http.StatusBadGateway, openai.CodeUpstreamAuth},
		{"cancel", context.Canceled, http.StatusRequestTimeout, openai.CodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return nil, tc.err }}
			rec := postChat(t, newTestServer(up), `{"model":"mercury-3","messages":[{"role":"user","content":"q"}]}`)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			var env openai.ErrorResponse
			json.Unmarshal(rec.Body.Bytes(), &env)
			if env.Error.Code != tc.code {
				t.Fatalf("code = %q, want %q", env.Error.Code, tc.code)
			}
		})
	}
}

func TestStreamErrorBeforeHeadersIsJSON(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return nil, errors.New("boom") }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "data: ") {
		t.Fatalf("pre-header failure emitted SSE: %s", rec.Body)
	}
}

func TestStreamMidStreamClientWriteFailure(t *testing.T) {
	t.Parallel()
	// A ResponseWriter that fails every write after the headers simulates a
	// client vanishing between frames.
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) {
		body := strings.Repeat("data: {\"type\":\"text-delta\",\"id\":\"text-0\",\"delta\":\"x\"}\n\n", 5) + "data: [DONE]\n\n"
		return streamOf(body), nil
	}}
	s := newTestServer(up)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"q"}]}`))
	rec := &failingWriter{header: http.Header{}, limit: 2}
	s.Handler().ServeHTTP(rec, req)
	if !rec.failed {
		t.Fatal("writer never failed; test did not exercise the path")
	}
}

// failingWriter succeeds for limit writes, then fails.
type failingWriter struct {
	header http.Header
	limit  int
	writes int
	failed bool
	status int
}

func (f *failingWriter) Header() http.Header { return f.header }
func (f *failingWriter) WriteHeader(status int) {
	f.status = status
}
func (f *failingWriter) Write(p []byte) (int, error) {
	if f.writes >= f.limit {
		f.failed = true
		return 0, errors.New("client gone")
	}
	f.writes++
	return len(p), nil
}

func TestHealthzMethodNotAllowed(t *testing.T) {
	t.Parallel()
	s := newTestServer(&fakeChatter{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestModelsMethodNotAllowed(t *testing.T) {
	t.Parallel()
	s := newTestServer(&fakeChatter{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/models", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
}

// TestStreamOnCapturedHardStream replays the real 18-event hard reasoning
// stream (audit capture) through the HTTP handler and asserts the ordering
// that fixes the 17.5 s zero-byte TTFT: reasoning frames arrive before any
// content frame, and no frame is lost.
func TestStreamOnCapturedHardStream(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../upstream/testdata/hard_stream.sse")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(string(raw)), nil }}
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"prove it"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	frames := sseFrames(t, rec.Body.String())

	var reasoningFrames, contentFrames int
	firstContent := -1
	lastReasoning := -1
	for i, f := range frames {
		if f == "[DONE]" {
			continue
		}
		c := decodeChunk(t, f)
		if c.Choices[0].Delta.ReasoningContent != "" {
			reasoningFrames++
			lastReasoning = i
		}
		if c.Choices[0].Delta.Content != "" {
			contentFrames++
			if firstContent < 0 {
				firstContent = i
			}
		}
	}
	if reasoningFrames != 4 {
		t.Fatalf("reasoning frames = %d, want 4", reasoningFrames)
	}
	if contentFrames != 10 {
		t.Fatalf("content frames = %d, want 10 text deltas", contentFrames)
	}
	if lastReasoning > firstContent {
		t.Fatalf("reasoning frame %d arrived after content frame %d", lastReasoning, firstContent)
	}
}

func TestBodyTooLarge(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{}
	big := strings.Repeat("x", maxBodyBytes+1024)
	rec := postChat(t, newTestServer(up), `{"model":"mercury-3","messages":[{"role":"user","content":"`+big+`"}]}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if up.count() != 0 {
		t.Fatal("oversized body reached upstream")
	}
}

// A message whose only part is empty has no renderable text: that is the one
// content rejection, because the upstream parts format cannot express it.
func TestEmptyOnlyContentRejected(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{}
	body := `{"model":"mercury-3","messages":[{"role":"user","content":[{"type":"text","text":""}]}]}`
	rec := postChat(t, newTestServer(up), body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if up.count() != 0 {
		t.Fatal("empty message reached upstream")
	}
}

// An empty part alongside real text is simply ignored; a large part count is
// forwarded, not capped by an invented policy.
func TestTextPartFloodPassesThrough(t *testing.T) {
	t.Parallel()
	up := &fakeChatter{fn: func(int) (*upstream.Stream, error) { return streamOf(hardStream), nil }}
	var parts strings.Builder
	parts.WriteString(`{"type":"text","text":""}`)
	for i := 0; i < 5000; i++ {
		parts.WriteString(`,{"type":"text","text":"x"}`)
	}
	body := `{"model":"mercury-3","messages":[{"role":"user","content":[` + parts.String() + `]}]}`
	rec := postChat(t, newTestServer(up), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	if got := len(up.lastRequest().Messages[0].Parts); got != 5000 {
		t.Fatalf("forwarded parts = %d, want 5000", got)
	}
}

// fakeClearance is a fixed clearance snapshot for /healthz.
type fakeClearance struct{ st clear.State }

func (f fakeClearance) State() clear.State { return f.st }

func TestHealthzReportsClearance(t *testing.T) {
	t.Parallel()
	s := newTestServer(&fakeChatter{})
	s.SetClearance(fakeClearance{st: clear.State{Live: 2, Pool: 3, HasCookie: true, AgeSeconds: 12, RefreshCount: 3,
		Slots: []clear.SlotView{
			{Cookie: true, State: clear.SlotLive, AgeSeconds: 12, RefreshCount: 1},
			{Cookie: true, State: clear.SlotLive, AgeSeconds: 8, RefreshCount: 1},
			{Cookie: false, State: clear.SlotPending, RefreshCount: 1},
		}}})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Clearance clear.State `json:"clearance"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Clearance.HasCookie || body.Clearance.AgeSeconds != 12 || body.Clearance.RefreshCount != 3 {
		t.Fatalf("clearance = %+v", body.Clearance)
	}
	// live/pool is the operator's one-line view of the ring filling to size.
	if body.Clearance.Live != 2 || body.Clearance.Pool != 3 {
		t.Fatalf("live/pool = %d/%d, want 2/3", body.Clearance.Live, body.Clearance.Pool)
	}
	// Per-slot detail is what lets an operator see one retired slot against a
	// healthy ring (SPEC §2.1.2).
	if len(body.Clearance.Slots) != 3 {
		t.Fatalf("slots = %+v, want 3", body.Clearance.Slots)
	}
	want := []clear.SlotView{
		{Cookie: true, State: clear.SlotLive, AgeSeconds: 12, RefreshCount: 1},
		{Cookie: true, State: clear.SlotLive, AgeSeconds: 8, RefreshCount: 1},
		{Cookie: false, State: clear.SlotPending, RefreshCount: 1},
	}
	for i := range want {
		if body.Clearance.Slots[i] != want[i] {
			t.Fatalf("slot %d = %+v, want %+v", i, body.Clearance.Slots[i], want[i])
		}
	}
	if !strings.Contains(rec.Body.String(), `"slots"`) {
		t.Fatalf("healthz body has no slots field: %s", rec.Body)
	}
}

func TestHealthzOmitsClearanceWithoutSource(t *testing.T) {
	t.Parallel()
	s := newTestServer(&fakeChatter{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	if _, ok := body["clearance"]; ok {
		t.Fatal("clearance must be omitted when no source is attached")
	}
}

// The mid-stream error frame is the same table as the pre-header envelope
// (SPEC §3.2): each upstream class maps to its stable code.
func TestStreamErrorCodes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		code string
	}{
		{"idle", upstream.ErrIdleTimeout, openai.CodeUpstreamIdle},
		{"truncated", upstream.ErrStreamTruncated, openai.CodeUpstreamStream},
		{"cancel", context.Canceled, openai.CodeUpstreamStream},
		{"other", errors.New("boom"), openai.CodeUpstreamStream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := streamError(tc.err).Code; got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
		})
	}
}
