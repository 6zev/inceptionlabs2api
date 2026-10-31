package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/upstream"
)

// getModels calls /v1/models with a valid key and decodes the envelope.
func getModels(t *testing.T, s *Server) openai.ModelList {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var list openai.ModelList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v (body %s)", err, w.Body.String())
	}
	return list
}

// TestModelsAdvertisingIsStable is the regression guard for the capability
// payload: a client cannot offer a reasoning-level selector unless these
// fields are present, which is the whole reason reasoning_content was never
// surfaced.
//
// The listing is exactly the configured surface: no derived "<name>-thinking"
// alias is added, because the reasoning capability is advertised here instead
// of being smuggled through the id.
func TestModelsAdvertisingIsStable(t *testing.T) {
	s := newTestServer(t, &mockChatter{})
	list := getModels(t, s)

	// newTestServer configures two names; both must appear and neither may
	// gain a derived "<name>-thinking" alias.
	if len(list.Data) != 2 {
		t.Fatalf("got %d models, want 2", len(list.Data))
	}
	for _, m := range list.Data {
		if strings.HasSuffix(m.ID, "-thinking") {
			t.Errorf("models listing contains a derived alias: %q", m.ID)
		}
	}
	for _, m := range list.Data {
		if !m.SupportsReasoning || !m.Capabilities.Reasoning {
			t.Errorf("%s: reasoning not advertised (%+v)", m.ID, m.Capabilities)
		}
		if !m.Capabilities.Streaming || !m.Capabilities.Tools {
			t.Errorf("%s: streaming/tools not advertised: %+v", m.ID, m.Capabilities)
		}
		if len(m.ReasoningLevels) != 4 {
			t.Errorf("%s: reasoning_levels = %v, want 4 levels", m.ID, m.ReasoningLevels)
		}
		// No Searcher is attached, so gateway-served search is off.
		if m.Capabilities.WebSearch {
			t.Errorf("%s: web_search advertised without a searcher", m.ID)
		}
	}
}

// TestModelsLimitsAreMeasurements pins the two limit families to their
// sources. They answer different questions and both must be right:
//
//   - limits.max_messages / max_input_bytes are what this deployment actually
//     enforces (measured), and
//   - context_length / top_provider are the model's published window, which is
//     what a client budgets tokens against.
func TestModelsLimitsAreMeasurements(t *testing.T) {
	s := newTestServer(t, &mockChatter{})
	m := getModels(t, s).Data[0]

	if m.Limits.MaxMessages != upstream.MaxUpstreamMessages {
		t.Errorf("max_messages = %d, want the upstream hard cap %d when unset",
			m.Limits.MaxMessages, upstream.MaxUpstreamMessages)
	}
	if m.Limits.MaxInputBytes != upstream.MaxInputBytes {
		t.Errorf("max_input_bytes = %d, want %d", m.Limits.MaxInputBytes, upstream.MaxInputBytes)
	}
	if m.ContextLength != upstream.MaxContextTokens {
		t.Errorf("context_length = %d, want the published window %d",
			m.ContextLength, upstream.MaxContextTokens)
	}
	if m.TopProvider.ContextLength != upstream.MaxContextTokens {
		t.Errorf("top_provider.context_length = %d, want %d",
			m.TopProvider.ContextLength, upstream.MaxContextTokens)
	}
	if m.TopProvider.MaxCompletionTokens != upstream.MaxOutputTokens {
		t.Errorf("top_provider.max_completion_tokens = %d, want %d",
			m.TopProvider.MaxCompletionTokens, upstream.MaxOutputTokens)
	}

	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Every key Chatbox's listModels looks at. A missing architecture block is
	// the one that silently drops the capability list entirely.
	for _, required := range []string{
		"context_length", "architecture", "supported_parameters", "pricing", "top_provider",
	} {
		if _, ok := fields[required]; !ok {
			t.Errorf("%s is missing: clients that read the OpenRouter shape will not see the capability", required)
		}
	}
}

// TestModelsParseLikeChatbox walks the payload through the mapping Chatbox
// applies to a custom OpenAI-compatible provider (fetchRemoteModels in
// src/shared/models/openai-compatible.ts) and asserts what its UI then knows.
//
// Without architecture the client builds no capability list at all, and
// without context_length it shows no window — which is exactly the report this
// test exists to keep fixed.
func TestModelsParseLikeChatbox(t *testing.T) {
	s := newTestServer(t, &mockChatter{})
	m := getModels(t, s).Data[0]

	capabilities := map[string]bool{}
	if m.Architecture.InputModalities != nil {
		for _, mod := range m.Architecture.InputModalities {
			if mod == "image" {
				capabilities["vision"] = true
			}
		}
		if m.Pricing.WebSearch != "" && m.Pricing.WebSearch != "0" {
			capabilities["web_search"] = true
		}
		for _, p := range m.SupportedParameters {
			switch p {
			case "reasoning", "include_reasoning", "reasoning_effort":
				capabilities["reasoning"] = true
			}
		}
	}

	if !capabilities["reasoning"] {
		t.Errorf("Chatbox would not offer thinking: supported_parameters = %v", m.SupportedParameters)
	}
	if capabilities["vision"] {
		t.Errorf("Chatbox would offer image input, but the backend is text-only")
	}
	if m.ContextLength <= 0 {
		t.Fatalf("Chatbox would show no context window (context_length = %d)", m.ContextLength)
	}
	if m.TopProvider.MaxCompletionTokens <= 0 || m.TopProvider.MaxCompletionTokens > m.ContextLength {
		t.Errorf("max_completion_tokens = %d is not a sane output cap for a %d-token window",
			m.TopProvider.MaxCompletionTokens, m.ContextLength)
	}
}

// TestModelsLimitReportsConfiguredCap covers the production wiring, where
// config.Defaults() sets MaxMessages=96, so the advertised cap is the trimmed
// one rather than the upstream's 100.
func TestModelsLimitReportsConfiguredCap(t *testing.T) {
	s := New(Config{
		APIKey:      "test-key",
		Model:       "mercury-3",
		MaxMessages: 96,
		IdleTimeout: 5 * 1e9,
		Logf:        func(string, ...any) {},
	}, &mockChatter{}, nil)

	m := getModels(t, s).Data[0]
	if m.Limits.MaxMessages != 96 {
		t.Errorf("max_messages = %d, want the configured 96", m.Limits.MaxMessages)
	}
}

// TestModelsLimitDisabledFallsBackToUpstreamCap covers IL2API_MAX_MESSAGES=0,
// where the gateway does not trim and the upstream's own 100 applies.
func TestModelsLimitDisabledFallsBackToUpstreamCap(t *testing.T) {
	s := New(Config{
		APIKey:      "test-key",
		Model:       "mercury-3",
		MaxMessages: 0,
		IdleTimeout: 5 * 1e9,
		Logf:        func(string, ...any) {},
	}, &mockChatter{}, nil)

	m := getModels(t, s).Data[0]
	if m.Limits.MaxMessages != upstream.MaxUpstreamMessages {
		t.Errorf("max_messages = %d, want the upstream hard cap %d",
			m.Limits.MaxMessages, upstream.MaxUpstreamMessages)
	}
}
