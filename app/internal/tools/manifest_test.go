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
	if !strings.Contains(m.Preamble, "must call") {
		t.Fatalf("named choice did not instruct the model:\n%s", m.Preamble)
	}
}

func TestManifestRequiredInstruction(t *testing.T) {
	t.Parallel()
	m, err := BuildManifest([]Tool{weatherTool()}, Choice{Required: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if !strings.Contains(strings.ToLower(m.Preamble), "must call") {
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
	m, err := BuildManifest([]Tool{{Name: "x", Parameters: json.RawMessage(`{"type":`)}, {Name: "y"}}, Choice{Auto: true}, false)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if !strings.Contains(m.Preamble, `"name":"x","description":"","parameters":{}`) {
		t.Fatalf("preamble:\n%s", m.Preamble)
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
