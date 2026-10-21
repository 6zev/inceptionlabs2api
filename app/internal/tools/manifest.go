package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// TierThreshold is the declared-tool count above which the manifest drops
// parameter schemas and offers the loader. 30 is the lower bound of the
// published 30–50-tool degradation range (DESIGN-TOOLS.md §6.2).
const TierThreshold = 30

// toolSchemaName is the synthetic loader's name. It is never a client tool:
// the server answers it itself and never emits it to the client.
const toolSchemaName = "tool_schema"

// maxExpand bounds how many tools one loader call may load, so a model cannot
// ask for the whole catalogue at once and defeat the point of tiering.
const maxExpand = 10

// Tool is one declared function, already validated by the request decoder.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Choice is tool_choice normalised by the decoder. Exactly one shape is set;
// Auto is also the zero value (OpenAI's default when tools are present).
type Choice struct {
	Auto     bool
	Required bool
	Named    string
}

// Manifest is the prompt material and the declared-name set for one request.
type Manifest struct {
	// Preamble is the system message placed before the client's messages.
	Preamble string
	// Declared is the set of names a call may use, in manifest order.
	Declared []string
	// Tiered reports that parameter schemas were withheld.
	Tiered bool
	// Loader reports that the synthetic tool_schema function was offered.
	Loader bool

	tools        []Tool
	instructions string
	// loaded is the subset of tools whose full schemas the current preamble
	// carries instead of the tier-1 index entry.
	loaded []Tool
	// opening is the prose clause after "You have access to", naming the
	// operations without the words "function"/"functions".
	opening string
}

// openingClause names the catalogue for the preamble. It deliberately avoids
// the word "function", which the probes tied to empty answers.
func openingClause(n int) string {
	if n == 1 {
		return "one operation, described below"
	}
	return fmt.Sprintf("%d operations, described below", n)
}

// openingFor names a single operation concretely in the prose; multi-tool
// requests keep the generic count.

// definitionHeader introduces the catalogue. Definitions are plain JSON lines:
// wrapping them in <tools>…</tools> made the upstream return an empty answer
// in 10/10 probes (DESIGN-TOOLS.md §1.1).
const definitionHeader = "\nOperation definitions, one JSON object per line:\n"

// tierNote explains why schemas are absent and how to ask for them.
const tierNote = "\nSome argument details are omitted to save context. If you need them, reply with a `" + toolSchemaName + "` block once: " + `{"name": "` + toolSchemaName + `", "arguments": {"names": ["…"]}}` + ".\n"

// header is the fixed part of every preamble: the call contract followed by
// the choice-specific instructions.
//
// The wording is not free-form: the upstream web backend intermittently
// returns an empty answer (reasoning-start/end only, no text-delta) when a
// system message looks like tool-calling scaffolding. The one reproducible
// trigger found in ~20 probes was a "<tools>…</tools>" catalogue block, which
// produced no text in 10/10 attempts; with the definitions as plain JSON lines
// the same text answered in 5/7. Headings and the word "function" were
// suspected first and later disproved as deterministic triggers. The exact
// phrasing is therefore part of the protocol, not copy: re-probe before
// rewording (DESIGN-TOOLS.md §1.1).
func (m *Manifest) header() string {
	example := m.example()
	return "You have access to " + m.opening + ". " +
		"When the user's request needs one, reply with exactly:\n\n" +
		callStart + "\n" +
		example + "\n" +
		callEnd + "\n\n" +
		"Use only the operation names listed below; never invent one. " +
		"Otherwise answer normally.\n" +
		m.instructions
}

// example renders the call block's example. With one operation it uses that
// operation's real name and first parameter, because the concrete form
// answered where a placeholder form returned an empty answer repeatedly
// (DESIGN-TOOLS.md §1.1). With several, a placeholder is the only truthful
// example.
func (m *Manifest) example() string {
	if len(m.tools) == 1 {
		t := m.tools[0]
		arg := "value"
		if p := firstProperty(t.Parameters); p != "" {
			arg = p
		}
		return `{"name": ` + quoteJSON(t.Name) + `, "arguments": {` + quoteJSON(arg) + `: "…"}}`
	}
	return `{"name": "<operation>", "arguments": {"<arg>": "…"}}`
}

// firstProperty returns the first declared parameter name, or "".
func firstProperty(raw json.RawMessage) string {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(raw, &schema) != nil || len(schema.Properties) == 0 {
		return ""
	}
	keys := make([]string, 0, len(schema.Properties))
	for k := range schema.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys[0]
}

// instructions renders the tool_choice / parallel instructions.
func instructions(c Choice, parallelOff bool) string {
	var b strings.Builder
	switch {
	case c.Named != "":
		fmt.Fprintf(&b, "\nYou must use the operation %q. Do not use any other operation.\n", c.Named)
	case c.Required:
		b.WriteString("\nYou MUST use at least one operation before answering.\n")
	}
	if parallelOff {
		b.WriteString("\nUse at most one operation per turn.\n")
	}
	return b.String()
}

// BuildManifest renders the tool preamble. parallelOff adds the "at most one"
// instruction. It returns an error only for a tool with a blank name, which
// the decoder rejects with a 400 before this point; the check keeps the
// package safe to call on its own.
func BuildManifest(list []Tool, c Choice, parallelOff bool) (*Manifest, error) {
	selected := list
	if c.Named != "" {
		selected = nil
		for _, t := range list {
			if t.Name == c.Named {
				selected = append(selected, t)
				break
			}
		}
		if len(selected) == 0 {
			// The caller asked for a tool it did not declare. Honour the
			// instruction (the client's own inconsistency) with a name-only
			// entry so the model can still emit the requested call.
			selected = []Tool{{Name: c.Named}}
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("tools: manifest needs at least one tool")
	}
	for _, t := range selected {
		if strings.TrimSpace(t.Name) == "" {
			return nil, fmt.Errorf("tools: tool name must not be empty")
		}
	}

	m := &Manifest{Tiered: len(selected) > TierThreshold, tools: selected, opening: openingClause(len(selected))}
	for _, t := range selected {
		m.Declared = append(m.Declared, t.Name)
	}

	m.instructions = instructions(c, parallelOff)

	var b strings.Builder
	b.WriteString(m.header())

	if m.Tiered {
		m.Loader = true
		b.WriteString(tierNote)
		b.WriteString(definitionHeader)
		for _, t := range selected {
			fmt.Fprintf(&b, `{"name":%s,"description":%s}`+"\n", quoteJSON(t.Name), quoteJSON(t.Description))
		}
		// Declare the loader last so it never shadows a real tool's name.
		m.Declared = append(m.Declared, toolSchemaName)
	} else {
		b.WriteString(definitionHeader)
		for _, t := range selected {
			fmt.Fprintf(&b, `{"name":%s,"description":%s,"parameters":%s}`+"\n",
				quoteJSON(t.Name), quoteJSON(t.Description), compactParams(t.Parameters))
		}
	}

	m.Preamble = b.String()
	return m, nil
}

// Expand answers a loader call's arguments with the tools it names, matched by
// exact name first and then by a case-insensitive lexical match over name and
// description. Results are capped at maxExpand. Unknown names are dropped.
func (m *Manifest) Expand(arguments string) []Tool {
	var req struct {
		Names []string `json:"names"`
		Query string   `json:"query"`
	}
	if err := json.Unmarshal([]byte(arguments), &req); err != nil {
		return nil
	}
	var out []Tool
	seen := make(map[string]struct{})
	add := func(t Tool) {
		if len(out) >= maxExpand {
			return
		}
		if _, dup := seen[t.Name]; dup {
			return
		}
		seen[t.Name] = struct{}{}
		out = append(out, t)
	}

	byName := make(map[string]Tool, len(m.tools))
	for _, t := range m.tools {
		byName[t.Name] = t
	}
	for _, n := range req.Names {
		if t, ok := byName[n]; ok {
			add(t)
		}
	}
	if q := strings.ToLower(strings.TrimSpace(req.Query)); q != "" {
		var hits []Tool
		for _, t := range m.tools {
			if strings.Contains(strings.ToLower(t.Name), q) || strings.Contains(strings.ToLower(t.Description), q) {
				hits = append(hits, t)
			}
		}
		sort.SliceStable(hits, func(i, j int) bool {
			// Name matches rank above description-only matches.
			return strings.Contains(strings.ToLower(hits[i].Name), q) && !strings.Contains(strings.ToLower(hits[j].Name), q)
		})
		for _, t := range hits {
			add(t)
		}
	}
	return out
}

// compactParams renders a parameter schema compactly, or {} when it is absent
// or not valid JSON.
func compactParams(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(trimmed)); err != nil {
		return "{}"
	}
	return buf.String()
}

// quoteJSON renders s as a JSON string literal.
func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Expanded returns the manifest for the next round: the original declared
// tools, with the given tools carrying full schemas instead of the tier-1
// index. Already-expanded tools keep their schemas. The loader stays offered
// so the model may ask again, but the server caps the number of rounds.
func (m *Manifest) Expanded(extra []Tool) *Manifest {
	// loaded keeps the tools whose schemas are now in the prompt.
	loaded := make(map[string]Tool, len(extra))
	for _, t := range m.loaded {
		loaded[t.Name] = t
	}
	for _, t := range extra {
		loaded[t.Name] = t
	}

	var b strings.Builder
	b.WriteString(m.header())
	b.WriteString(tierNote)
	b.WriteString(definitionHeader)
	for _, t := range m.tools {
		if full, ok := loaded[t.Name]; ok {
			fmt.Fprintf(&b, `{"name":%s,"description":%s,"parameters":%s}`+"\n",
				quoteJSON(full.Name), quoteJSON(full.Description), compactParams(full.Parameters))
			continue
		}
		fmt.Fprintf(&b, `{"name":%s,"description":%s}`+"\n", quoteJSON(t.Name), quoteJSON(t.Description))
	}

	declared := make([]string, 0, len(m.tools)+1)
	for _, t := range m.tools {
		declared = append(declared, t.Name)
	}
	declared = append(declared, toolSchemaName)

	allLoaded := make([]Tool, 0, len(loaded))
	for _, t := range m.tools {
		if full, ok := loaded[t.Name]; ok {
			allLoaded = append(allLoaded, full)
		}
	}

	return &Manifest{
		Preamble:     b.String(),
		Declared:     declared,
		Tiered:       true,
		Loader:       true,
		tools:        m.tools,
		instructions: m.instructions,
		loaded:       allLoaded,
		opening:      m.opening,
	}
}
