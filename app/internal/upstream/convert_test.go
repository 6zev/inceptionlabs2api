package upstream

import (
	"encoding/json"
	"regexp"
	"testing"

	"inceptionlabs2api/internal/openai"
)

func TestNewChatRequestShape(t *testing.T) {
	t.Parallel()
	req := NewChatRequest([]openai.TextMessage{
		{Role: "system", Texts: []string{"be brief"}},
		{Role: "user", Texts: []string{"hello", "world"}},
		{Role: "assistant", Texts: []string{"hi"}},
	}, "high")

	if req.ReasoningEffort != "high" || !req.WebSearchEnabled || req.VoiceMode {
		t.Errorf("fixed fields wrong: %+v", req)
	}
	if req.Timezone != "UTC" || req.Language != "en-US" || req.Trigger != "submit-message" {
		t.Errorf("web-client defaults wrong: %+v", req)
	}
	if !regexp.MustCompile(`^c-[0-9a-f]{12}$`).MatchString(req.ID) {
		t.Errorf("id = %q, want c-<12 hex>", req.ID)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Parts[0].State != "" {
		t.Errorf("system message = %+v, want no state", req.Messages[0])
	}
	if len(req.Messages[1].Parts) != 2 || req.Messages[1].Parts[1].Text != "world" {
		t.Errorf("user parts = %+v", req.Messages[1].Parts)
	}
	if req.Messages[2].Parts[0].State != "done" {
		t.Errorf("assistant part state = %q, want done", req.Messages[2].Parts[0].State)
	}
	for _, m := range req.Messages {
		if !regexp.MustCompile(`^m-[0-9a-f]{12}$`).MatchString(m.ID) {
			t.Errorf("message id = %q, want m-<12 hex>", m.ID)
		}
	}
}

func TestNewChatRequestDefaultsEffort(t *testing.T) {
	t.Parallel()
	req := NewChatRequest([]openai.TextMessage{{Role: "user", Texts: []string{"hi"}}}, "")
	if req.ReasoningEffort != "medium" {
		t.Fatalf("effort = %q, want medium", req.ReasoningEffort)
	}
}

func TestNewChatRequestJSONKeys(t *testing.T) {
	t.Parallel()
	req := NewChatRequest([]openai.TextMessage{{Role: "user", Texts: []string{"hi"}}}, "low")
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"reasoningEffort", "webSearchEnabled", "voiceMode", "timezone", "language", "id", "messages", "trigger"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing key %q in %s", key, b)
		}
	}
	if raw["webSearchEnabled"] != true || raw["voiceMode"] != false {
		t.Errorf("booleans = %v/%v, want false", raw["webSearchEnabled"], raw["voiceMode"])
	}
	msg := raw["messages"].([]any)[0].(map[string]any)
	if _, ok := msg["role"]; !ok {
		t.Errorf("message missing role: %v", msg)
	}
}

func TestRandomIDsAreDistinct(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := RandomID("x")
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
