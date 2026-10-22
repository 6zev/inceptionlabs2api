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

// T24: at or below the threshold, every parameter schema is present and no
// loader is offered.
func TestManifestFullBelowThreshold(t *testing.T) {
	t.Parallel()
	tools := []Tool{weatherTool()}
	m, err := BuildManifest(tools, Choice{Auto: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m.Tiered || m.Loader {
		t.Fatalf("tiered=%v loader=%v below threshold", m.Tiered, m.Loader)
	}
	if !strings.Contains(m.Preamble, `"parameters"`) || !strings.Contains(m.Preamble, "get_weather") {
		t.Fatalf("preamble lacks schema:\n%s", m.Preamble)
	}
	if strings.Contains(m.Preamble, "tool_schema") {
		t.Fatal("loader advertised below threshold")
	}
}

// T21: above the threshold the preamble carries names and descriptions only,
// plus the loader.
func TestManifestTieredAboveThreshold(t *testing.T) {
	t.Parallel()
	tools := manyTools(TierThreshold + 1)
	m, err := BuildManifest(tools, Choice{Auto: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if !m.Tiered || !m.Loader {
		t.Fatalf("tiered=%v loader=%v, want both", m.Tiered, m.Loader)
	}
	if strings.Contains(m.Preamble, `"parameters"`) {
		t.Fatal("tier-1 manifest still carries parameter schemas")
	}
	for _, tool := range tools {
		if !strings.Contains(m.Preamble, tool.Name) {
			t.Fatalf("tool %q missing from index", tool.Name)
		}
	}
	if !strings.Contains(m.Preamble, toolSchemaName) {
		t.Fatal("loader not in preamble")
	}
	if len(m.Declared) != len(tools)+1 || m.Declared[len(m.Declared)-1] != toolSchemaName {
		t.Fatalf("declared = %v", m.Declared)
	}
}

func TestManifestThresholdBoundary(t *testing.T) {
	t.Parallel()
	m, err := BuildManifest(manyTools(TierThreshold), Choice{Auto: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m.Tiered {
		t.Fatalf("tiered at exactly %d tools; threshold is >%d", TierThreshold, TierThreshold)
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

// Expanding a loader call returns the requested tools, matched by name first
// and by lexical query second.
func TestExpand(t *testing.T) {
	t.Parallel()
	tools := manyTools(5)
	m, err := BuildManifest(tools, Choice{Auto: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	got := m.Expand(`{"names":["tool_1","tool_3"]}`)
	if len(got) != 2 || got[0].Name != "tool_1" || got[1].Name != "tool_3" {
		t.Fatalf("names expansion = %v", got)
	}

	got = m.Expand(`{"query":"tool_2"}`)
	if len(got) == 0 || got[0].Name != "tool_2" {
		t.Fatalf("query expansion = %v", got)
	}

	if got := m.Expand(`not json`); len(got) != 0 {
		t.Fatalf("garbage expanded to %v", got)
	}
	if got := m.Expand(`{"names":["missing"]}`); len(got) != 0 {
		t.Fatalf("unknown name expanded to %v", got)
	}
}

func TestExpandCapsResults(t *testing.T) {
	t.Parallel()
	m, err := BuildManifest(manyTools(TierThreshold+20), Choice{Auto: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if got := m.Expand(`{"query":"tool"}`); len(got) > maxExpand {
		t.Fatalf("expanded %d > cap %d", len(got), maxExpand)
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

// Expanded turns named tools into full schemas while keeping every other
// tool's index entry and the loader, and stays declared.
func TestExpanded(t *testing.T) {
	t.Parallel()
	tools := manyTools(TierThreshold + 2)
	m, err := BuildManifest(tools, Choice{Auto: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	first := m.tools[0]
	next := m.Expanded([]Tool{first})
	if !next.Tiered || !next.Loader {
		t.Fatalf("expanded tiered=%v loader=%v", next.Tiered, next.Loader)
	}
	if !strings.Contains(next.Preamble, `"name":"tool_0","description":"Operation number 0 for testing.","parameters":{"type":"object"`) {
		t.Fatalf("expanded tool lacks schema:\n%s", next.Preamble)
	}
	// An unloaded tool keeps its index entry: expanding one tool must not
	// re-inflate the whole manifest.
	if !strings.Contains(next.Preamble, `{"name":"tool_1","description":"Operation number 1 for testing."}`+"\n") {
		t.Fatalf("unloaded tool lost its index entry:\n%s", next.Preamble)
	}
	if strings.Count(next.Preamble, `,"parameters":`) != 1 {
		t.Fatalf("expanded manifest carries %d schemas, want 1", strings.Count(next.Preamble, `,"parameters":`))
	}
	// Every declared name must still be declared, plus the loader.
	if len(next.Declared) != len(tools)+1 || next.Declared[len(next.Declared)-1] != toolSchemaName {
		t.Fatalf("declared = %v", next.Declared)
	}
	if len(next.tools) != len(tools) {
		t.Fatalf("tools = %d", len(next.tools))
	}
	// Unknown tools cannot be injected into the catalogue: Expand only
	// answers with declared tools, and a second expansion keeps the first
	// tool's schema while the index stays complete.
	next = next.Expanded([]Tool{{Name: "extra", Parameters: json.RawMessage(`{}`)}})
	if len(next.tools) != len(tools) {
		t.Fatalf("tools after extra = %d", len(next.tools))
	}
	if !strings.Contains(next.Preamble, `"name":"tool_0"`) || !strings.Contains(next.Preamble, `"name":"tool_30"`) {
		t.Fatalf("index incomplete after second expansion")
	}
	if strings.Contains(next.Preamble, `"name":"extra"`) {
		t.Fatal("undeclared tool was injected into the catalogue")
	}
	if strings.Count(next.Preamble, `,"parameters":`) != 1 {
		t.Fatalf("second expansion dropped the first schema")
	}
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
