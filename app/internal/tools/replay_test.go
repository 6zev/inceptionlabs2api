package tools

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// T14: assistant calls plus consecutive tool results replay as one assistant
// turn and one grouped user turn.
func TestRenderAssistantAndResults(t *testing.T) {
	t.Parallel()
	assistant := RenderAssistant("Checking both.", []EncodedCall{
		{ID: "call_a", Name: "get_weather", Arguments: `{"city":"Paris"}`},
		{ID: "call_b", Name: "get_time", Arguments: `{"zone":"CET"}`},
	})
	if !strings.Contains(assistant, `{"id":"call_a","name":"get_weather","arguments":{"city":"Paris"}}`) {
		t.Fatalf("assistant = %q", assistant)
	}
	if strings.Count(assistant, callStart) != 2 {
		t.Fatalf("assistant = %q", assistant)
	}
	if !strings.HasPrefix(assistant, "Checking both.") {
		t.Fatalf("text lost: %q", assistant)
	}

	results := RenderResults([]Result{
		{CallID: "call_a", Name: "get_weather", Content: `{"tempC":21}`},
		{CallID: "call_b", Name: "get_time", Content: "14:00"},
	})
	if strings.Count(results, "<tool_response") != 2 {
		t.Fatalf("results = %q", results)
	}
	if !strings.Contains(results, `<tool_response call_id="call_a" name="get_weather">`) {
		t.Fatalf("results = %q", results)
	}
	if strings.Index(results, "call_a") > strings.Index(results, "call_b") {
		t.Fatal("results not in arrival order")
	}
}

// T15: a result whose id matches nothing keeps its id and omits the name.
func TestRenderResultWithoutMatchingCall(t *testing.T) {
	t.Parallel()
	out := RenderResults([]Result{{CallID: "call_orphan", Content: "x"}})
	if !strings.Contains(out, `call_id="call_orphan"`) || strings.Contains(out, "name=") {
		t.Fatalf("out = %q", out)
	}
}

// A result without an id is still rendered, never dropped.
func TestRenderResultWithoutID(t *testing.T) {
	t.Parallel()
	out := RenderResults([]Result{{Content: "bare"}})
	if !strings.Contains(out, "<tool_response>") || !strings.Contains(out, "bare") {
		t.Fatalf("out = %q", out)
	}
}

// Client-supplied garbage arguments must not produce an unparseable replay.
func TestRenderAssistantRepairsNonObjectArguments(t *testing.T) {
	t.Parallel()
	for _, args := range []string{"", "not json", `"[1,2]"`, `{"unterminated":`} {
		out := RenderAssistant("", []EncodedCall{{ID: "call_x", Name: "f", Arguments: args}})
		body := strings.TrimSuffix(strings.TrimPrefix(out, callStart), callEnd)
		var v map[string]any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("args %q: replay block invalid: %v (%s)", args, err, out)
		}
		if _, ok := v["arguments"].(string); !ok {
			t.Fatalf("args %q: expected string fallback, got %T", args, v["arguments"])
		}
	}
}

// Attribute values can never escape their quotes.
func TestRenderResultsEscapesAttributes(t *testing.T) {
	t.Parallel()
	out := RenderResults([]Result{{CallID: `x" injected="y`, Name: "f", Content: "c"}})
	if strings.Contains(out, `injected="y"`) {
		t.Fatalf("attribute injection: %q", out)
	}
	if !strings.Contains(out, "&quot;") {
		t.Fatalf("quote not escaped: %q", out)
	}
}

// T16: ids are OpenAI-shaped, unique, and opaque.
func TestNewID(t *testing.T) {
	t.Parallel()
	shape := regexp.MustCompile(`^call_[A-Za-z0-9]{24}$`)
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := NewID()
		if !shape.MatchString(id) {
			t.Fatalf("id = %q", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
}

// A rendered assistant turn round-trips through the parser: what we teach the
// model on replay is exactly what the parser would accept back.
func TestReplayRoundTripsThroughParser(t *testing.T) {
	t.Parallel()
	out := RenderAssistant("", []EncodedCall{{ID: "call_a", Name: "f", Arguments: `{"k":"v"}`}})
	p := NewParser([]string{"f"})
	content, calls := run(t, p, out)
	if content != "" || len(calls) != 1 || calls[0].Name != "f" {
		t.Fatalf("content=%q calls=%v", content, calls)
	}
	mustCall(t, calls[0].Arguments, "f", `{"k":"v"}`)
}
