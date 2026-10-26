package server

// Shared test scaffolding for the server package.
//
// This file was missing from the published tree: history_fold_test.go,
// no_tools_search_test.go, refused_recovery_test.go and search_answer_test.go
// all reference mockChatter, sseText, postChat, partTexts and newTestServer,
// but nothing defined them, so `go test ./internal/server` could never
// compile. The helpers below are reconstructed from those call sites.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/upstream"
)

// mockChatter scripts one SSE body per Chat call and records every request.
// Once the script is exhausted it fails loudly rather than silently returning
// an empty stream, so an unexpected extra upstream round is a test failure.
type mockChatter struct {
	mu     sync.Mutex
	rounds []string
	reqs   []*upstream.ChatRequest
	errs   []error
	// malformed records the onMalformed callbacks the server wires up.
	malformed []string
}

func (m *mockChatter) Chat(ctx context.Context, req *upstream.ChatRequest, idle time.Duration, onMalformed func(string)) (*upstream.Stream, error) {
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	if len(m.errs) > 0 {
		err := m.errs[0]
		m.errs = m.errs[1:]
		m.mu.Unlock()
		if err != nil {
			return nil, err
		}
	} else {
		m.mu.Unlock()
	}

	m.mu.Lock()
	if len(m.rounds) == 0 {
		m.mu.Unlock()
		return nil, errMockExhausted
	}
	body := m.rounds[0]
	m.rounds = m.rounds[1:]
	m.mu.Unlock()

	rc := &readCloser{Reader: strings.NewReader(body)}
	return &upstream.Stream{Body: rc, Reader: upstream.NewReader(rc, idle, onMalformed)}, nil
}

// count returns how many upstream rounds were requested.
func (m *mockChatter) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.reqs)
}

// last returns the most recent upstream request, or nil.
func (m *mockChatter) last() *upstream.ChatRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.reqs) == 0 {
		return nil
	}
	return m.reqs[len(m.reqs)-1]
}

// request returns the i-th upstream request (0-based), or nil.
func (m *mockChatter) request(i int) *upstream.ChatRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.reqs) {
		return nil
	}
	return m.reqs[i]
}

type errMock string

func (e errMock) Error() string { return string(e) }

const errMockExhausted = errMock("mock: no scripted upstream round left")

// readCloser adapts a strings.Reader to io.ReadCloser.
type readCloser struct{ *strings.Reader }

func (r *readCloser) Close() error { return nil }

// sseText encodes one whole answer as a single text-start snapshot frame
// followed by [DONE]. The upstream sends whole answers in one event on this
// diffusion model, so this is the realistic shape.
func sseText(text string) string {
	return sseEvent(upstream.Event{Type: upstream.EventTextStart, Snapshot: text})
}

// sseReasoning encodes one reasoning delta frame followed by [DONE].
func sseReasoning(text string) string {
	return sseEvent(upstream.Event{Type: upstream.EventReasoningDelta, Delta: text})
}

// sseEvent encodes a single event as an SSE data frame, without JSON HTML
// escaping (call markers must survive as raw bytes), terminated by [DONE].
func sseEvent(ev upstream.Event) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(ev)
	return "data: " + strings.TrimRight(b.String(), "\n") + "\n\ndata: [DONE]\n\n"
}

// sseEmpty encodes a clean stream that carries no text at all: the measured
// "empty completion" shape, ending in [DONE] rather than truncating.
func sseEmpty() string {
	return "data: " + `{"type":"text-end"}` + "\n\ndata: [DONE]\n\n"
}

// partTexts returns the text of every part of one upstream message.
func partTexts(m upstream.Message) []string {
	out := make([]string, 0, len(m.Parts))
	for _, p := range m.Parts {
		out = append(out, p.Text)
	}
	return out
}

// newTestServer builds a Server with no search backend and NativeWebSearch
// off, so a request takes exactly the legacy path.
func newTestServer(t *testing.T, up Chatter) *Server {
	t.Helper()
	return New(Config{
		APIKey:      "test-key",
		Model:       "mercury",
		Models:      []string{"mercury", "mercury-3"},
		IdleTimeout: 5 * time.Second,
		Logf:        func(string, ...any) {},
	}, up, nil)
}

// postChat posts body to /v1/chat/completions with a valid bearer key and
// returns the recorder.
func postChat(t *testing.T, s *Server, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer test-key")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// decodeCompletion decodes a non-streaming chat.completion response.
func decodeCompletion(t *testing.T, body []byte) openai.Completion {
	t.Helper()
	var c openai.Completion
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("decode completion: %v (body %q)", err, string(body))
	}
	return c
}

// streamFrames parses an SSE response body into its chat.completion.chunk
// frames. The terminal `data: [DONE]` sentinel is not a frame and is skipped.
func streamFrames(t *testing.T, body string) []openai.Chunk {
	t.Helper()
	var out []openai.Chunk
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		// A frame may also be an error envelope; only chunks are returned.
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payload), &probe); err != nil {
			t.Fatalf("decode sse frame %q: %v", payload, err)
		}
		if _, isChunk := probe["choices"]; !isChunk {
			continue
		}
		var c openai.Chunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("decode chunk %q: %v", payload, err)
		}
		out = append(out, c)
	}
	return out
}

// toolCallNames collects the tool-call names emitted across streamed frames.
func toolCallNames(chunks []openai.Chunk) []string {
	var out []string
	for _, c := range chunks {
		for _, tc := range c.Choices[0].Delta.ToolCalls {
			if tc.Function.Name != "" {
				out = append(out, tc.Function.Name)
			}
		}
	}
	return out
}

// contentOf concatenates every streamed content delta.
func contentOf(chunks []openai.Chunk) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(c.Choices[0].Delta.Content)
	}
	return b.String()
}

// reasoningOf concatenates every streamed reasoning delta.
func reasoningOf(chunks []openai.Chunk) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString(c.Choices[0].Delta.ReasoningContent)
	}
	return b.String()
}

// weatherRequest is the canonical tools-bearing chat body: one user question
// plus the get_weather schema, used by the tool-path tests.
func weatherRequest(stream bool) []byte {
	body := map[string]any{
		"model":  "mercury",
		"stream": stream,
		"messages": []map[string]any{
			{"role": "user", "content": "东京现在天气怎么样?"},
		},
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "Get current weather",
				"parameters":  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
			},
		}},
		"tool_choice": "auto",
	}
	b, _ := json.Marshal(body)
	return b
}
