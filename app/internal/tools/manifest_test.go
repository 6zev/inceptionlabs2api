package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

func weatherTool() Tool {
	return Tool{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string","description":"City name."}},"required":["city"]}`),
	}
}

// The manifest has no count-based tiering: a request declaring 40 tools
// carries all 40 full definitions, schemas included, and no loader note.
func TestManifestRendersFullDefinitionsAtAnyCount(t *testing.T) {
	t.Parallel()
	tools := manyTools(40)
	m, err := BuildManifest(tools, Choice{Auto: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if got := strings.Count(m.Preamble, `,"parameters":`); got != len(tools) {
		t.Fatalf("preamble carries %d schemas, want %d:\n%s", got, len(tools), m.Preamble)
	}
	for _, tool := range tools {
		if !strings.Contains(m.Preamble, tool.Name) {
			t.Fatalf("tool %q missing from preamble", tool.Name)
		}
	}
	if strings.Contains(m.Preamble, "tool_schema") {
		t.Fatal("loader advertised")
	}
	if len(m.Declared) != len(tools) {
		t.Fatalf("declared %d names, want %d", len(m.Declared), len(tools))
	}
}

// Compact, not pretty: no indentation bytes in the manifest.
func TestManifestIsCompact(t *testing.T) {
	t.Parallel()
	m, err := BuildManifest([]Tool{weatherTool()}, Choice{Auto: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if strings.Contains(m.Preamble, "\n  ") {
		t.Fatalf("preamble is indented:\n%s", m.Preamble)
	}
	// The winning skeleton instructs with the contract line, not a filled-in
	// example (the 14-probe matrix, 7/8 live hit, used no example block).
	if !strings.Contains(m.Preamble, `respond with a <tool_call> JSON block containing {"name": "tool name", "arguments": {…}}`) {
		t.Fatalf("tool contract missing:\\n%s", m.Preamble)
	}
}

// T12: a named choice lists only that tool, and only it is declared.
func TestManifestNamedChoice(t *testing.T) {
	t.Parallel()
	tools := []Tool{weatherTool(), {Name: "send_email", Description: "Send mail.", Parameters: json.RawMessage(`{}`)}}
	m, err := BuildManifest(tools, Choice{Named: "send_email"}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if strings.Contains(m.Preamble, "get_weather") {
		t.Fatal("named choice still lists the other tool")
	}
	if !strings.Contains(m.Preamble, "send_email") {
		t.Fatal("named tool missing")
	}
	if len(m.Declared) != 1 || m.Declared[0] != "send_email" {
		t.Fatalf("declared = %v", m.Declared)
	}
	if !strings.Contains(m.Preamble, "must use") {
		t.Fatalf("named choice did not instruct the model:\n%s", m.Preamble)
	}
}

func TestManifestRequiredInstruction(t *testing.T) {
	t.Parallel()
	m, err := BuildManifest([]Tool{weatherTool()}, Choice{Required: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if !strings.Contains(strings.ToLower(m.Preamble), "must use") {
		t.Fatalf("required instruction missing:\n%s", m.Preamble)
	}
}

// T25: parallel_tool_calls=false adds an instruction but never trims the list.
func TestManifestParallelOff(t *testing.T) {
	t.Parallel()
	m, err := BuildManifest([]Tool{weatherTool(), {Name: "b", Parameters: json.RawMessage(`{}`)}}, Choice{Auto: true}, true)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if !strings.Contains(strings.ToLower(m.Preamble), "at most one") {
		t.Fatalf("parallel instruction missing:\n%s", m.Preamble)
	}
	if len(m.Declared) != 2 {
		t.Fatalf("declared = %v", m.Declared)
	}
}

// A named tool that the request did not declare still produces an instruction
// (the client asked for it), but it cannot be executed, so it is declared and
// the model is told to call it. This is the caller's own inconsistency.
func TestManifestNamedUnknownTool(t *testing.T) {
	t.Parallel()
	m, err := BuildManifest([]Tool{weatherTool()}, Choice{Named: "ghost"}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if len(m.Declared) != 1 || m.Declared[0] != "ghost" {
		t.Fatalf("declared = %v", m.Declared)
	}
}

// Unparseable or absent parameters render as {}, never drop the tool.
func TestManifestBadParametersBecomeEmptyObject(t *testing.T) {
	t.Parallel()
	m, err := BuildManifest([]Tool{{Name: "x", Description: "d", Parameters: json.RawMessage(`{"type":`)}, {Name: "y", Description: "e"}}, Choice{Auto: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	// Bad parameters degrade to {}, and a present description stays.
	if !strings.Contains(m.Preamble, `{"name":"x","description":"d","parameters":{}}`) {
		t.Fatalf("preamble:\n%s", m.Preamble)
	}
}

// Several tools still carry the same contract line; no fabricated examples.
func TestManifestMultiToolExampleStaysGeneric(t *testing.T) {
	t.Parallel()
	m, err := BuildManifest([]Tool{weatherTool(), {Name: "send_email", Parameters: json.RawMessage(`{"properties":{"to":{}}}`)}}, Choice{Auto: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if !strings.Contains(m.Preamble, `respond with a <tool_call> JSON block containing {"name": "tool name", "arguments": {…}}`) {
		t.Fatalf("multi-tool preamble missing the contract line:\\n%s", m.Preamble)
	}
	for _, fabricated := range []string{`{"name": "get_weather", "arguments": {"city": "…"}}`, `"<operation>"`} {
		if strings.Contains(m.Preamble, fabricated) {
			t.Fatalf("fabricated example leaked into multi-tool preamble: %s", fabricated)
		}
	}
}

func TestManifestRejectsEmptyName(t *testing.T) {
	t.Parallel()
	if _, err := BuildManifest([]Tool{{Name: " "}}, Choice{Auto: true}, false); err == nil {
		t.Fatal("empty name accepted")
	}
}

func manyTools(n int) []Tool {
	out := make([]Tool, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Tool{
			Name:        "tool_" + itoa(i),
			Description: "Operation number " + itoa(i) + " for testing.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		})
	}
	return out
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}

// The native search tool is declared with a real parameter schema derived
// from the search backend's request shape: query is required, config is
// optional tuning.
func TestNativeSearchToolDefinition(t *testing.T) {
	t.Parallel()
	tool := NativeSearchTool()
	if tool.Name != NativeSearchName {
		t.Fatalf("name = %q", tool.Name)
	}
	if tool.Description == "" {
		t.Fatal("no description")
	}
	var schema struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(tool.Parameters, &schema); err != nil {
		t.Fatalf("parameters are not a schema: %v", err)
	}
	if schema.Type != "object" || schema.Properties["query"] == nil || schema.Properties["config"] == nil {
		t.Fatalf("schema = %s", tool.Parameters)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "query" {
		t.Fatalf("required = %v", schema.Required)
	}

	// The manifest renders it like any client tool, with the schema inline.
	m, err := BuildManifest([]Tool{tool}, Choice{Auto: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if !strings.Contains(m.Preamble, `"name":"web_search"`) || !strings.Contains(m.Preamble, `"properties"`) {
		t.Fatalf("preamble = %s", m.Preamble)
	}
	if len(m.Declared) != 1 || m.Declared[0] != NativeSearchName {
		t.Fatalf("declared = %v", m.Declared)
	}
}
