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

// TestModelsLimitsAreMeasurements pins the limits to what the upstream really
// enforces. It deliberately asserts absence of a token window: inventing one
// would make clients send requests the upstream rejects with 502.
//
// Note the MaxMessages value: server.New does not apply the config default
// (config.Defaults owns that), so a Server built without MaxMessages reports
// the upstream's own hard cap. That is the honest answer for that wiring.
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

	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, forbidden := range []string{"context_length", "context_window", "max_tokens"} {
		if _, ok := fields[forbidden]; ok {
			t.Errorf("%s must not be advertised: the upstream has no token window", forbidden)
		}
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
