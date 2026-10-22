package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// PreStyle selects the preamble skeleton. "hermes" is the measured 7/8
// hermes-compact skeleton; "inception" is the reconstructed AI-SDK v5 style
// the upstream chat itself runs under (probes outX/X1 first-words + outZ2/Z4
// verbatim identity line + outAA confirmed headings): identity sentence,
// numbered behavioral items with the call contract as item 1, and the
// code-outputs clause styled after the upstream's own item 4.
var PreStyle = "hermes"

// skeleton is one preamble wording option. Both skeletons render the same
// definition lines; only the prose differs.
type skeleton struct {
	lead     string // opening sentence before the definition lines
	contract string // the call-contract sentence after the definitions
}

// hermesSkeleton is the hermes-compact skeleton from the 14-probe wording
// matrix (7/8 live hit rate; probe/outP1..C). The previous "You have access
// to N operations..." draft missed 3/3 locally — do not reword without a new
// live matrix (DESIGN-TOOLS.md §1.1).
var hermesSkeleton = skeleton{
	lead: "You are a helpful assistant with access to the following tools:",
	contract: "\nIf you need to use a tool, respond with a " + callStart + " JSON block " +
		"containing {\"name\": \"tool name\", \"arguments\": {…}}. " +
		"Use only the tools listed; never invent one. Otherwise answer normally.",
}

// inceptionSkeleton mirrors the AI-SDK v5 structure the upstream chat itself
// runs under — measured reconstruction: identity sentence (outZ2/Z4 verbatim,
// "You are an AI assistant trained by Inception."), numbered behavioral items
// (outX/X1 first-words verbatim), no JSON blocks in prose, code-outputs clause
// styled after the upstream's item 4. The call contract replaces item 1 with
// the same "Prioritize." imperative cadence.
// NB: item text uses upstream-confirmed openers; bodies are ours. Not a copy
// of the upstream prompt — only its genre, numbering style and cadence.
var inceptionSkeleton = skeleton{
	lead: "# Tool Operations\n\n" +
		"1. Instruction Following: Prioritize using a listed operation whenever the user's request needs one, before answering in prose." +
		" Accuracy & Detail: use only the operations listed below; their argument schemas are the source of truth." +
		" Safety & Ethics: never invent an operation that is not listed." +
		" Code outputs: when you call an operation, the first characters of your reply must be the marker " + callStart +
		" followed by one JSON block containing {\"name\": \"tool name\", \"arguments\": {…}}, closed by " + callEnd +
		" — never write the block as plain text." +
		" Conversational Momentum: otherwise answer normally in prose.",
	contract: "\nEvery operation call uses the " + callStart + "..." + callEnd + " marker block; a bare JSON object without the marker is invalid.",
}

// Tool is one declared function, already validated by the request decoder.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// NativeSearchName is the gateway-served retrieval tool. It is offered as an
// ordinary manifest entry, so the model sees it exactly like a client tool;
// the server answers its calls itself and never emits them to the client.
const NativeSearchName = "web_search"

// NativeSearchTool is the definition of the gateway-served search tool. Its
// parameters mirror the search backend's wire request (query plus the
// search-lab config block); the server clamps and defaults them exactly as
// /v1/web_search does.
func NativeSearchTool() Tool {
	return Tool{
		Name:        NativeSearchName,
		Description: "Search the web for current information. Use it when the answer depends on facts you cannot verify from memory.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"query":{"type":"string","description":"The search query, phrased as a question or a few keywords."},` +
			`"config":{"type":"object","description":"Optional retrieval tuning; defaults are almost always right.",` +
			`"properties":{` +
			`"fanout":{"type":"integer","description":"Number of parallel query variants (1-5)."},` +
			`"depth":{"type":"integer","description":"Retrieval rounds (1-3)."},` +
			`"keep_top_k":{"type":"integer","description":"Sources to keep."},` +
			`"effort":{"type":"string","description":"Retrieval effort, e.g. instant."}` +
			`}}},` +
			`"required":["query"]}`),
	}
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

	tools        []Tool
	instructions string
}

// The preamble wording is not free-form: the upstream web backend
// intermittently returns an empty answer (reasoning-start/end only, no
// text-delta) when a system message looks like tool-calling scaffolding. The
// one reproducible trigger found in ~20 probes was a "<tools>…</tools>"
// catalogue block, which produced no text in 10/10 attempts; with the
// definitions as plain JSON lines the same text answered in 5/7. Headings and
// the word "function" were suspected first and later disproved as
// deterministic triggers. The exact phrasing is therefore part of the
// protocol, not copy: re-probe before rewording (DESIGN-TOOLS.md §1.1).

// activeSkeleton returns the skeleton selected by PreStyle, defaulting to
// hermes for any unknown value.
func activeSkeleton() skeleton {
	if PreStyle == "inception" {
		return inceptionSkeleton
	}
	return hermesSkeleton
}

func (m *Manifest) header() string {
	// The skeleton is selected by PreStyle; the hermes variant is the
	// measured 7/8 baseline (see hermesSkeleton).
	var defs strings.Builder
	for _, t := range m.tools {
		// An empty description renders natively: the code-lab catalog
		// (probe/outY/y1, verbatim) never carries a description field
		// without content, so an empty string becomes an omitted field
		// rather than literal "".
		params := ""
		if t.Description != "" {
			params = `,"description":` + quoteJSON(t.Description)
		}
		fmt.Fprintf(&defs, `{"name":%s%s,"parameters":%s}`+"\n",
			quoteJSON(t.Name), params, compactParams(t.Parameters))
	}
	sk := activeSkeleton()
	preamble := sk.lead + "\n\n" + defs.String() + sk.contract + "\n"
	return preamble + m.instructions
}

// The skeleton carries the call contract in prose; it deliberately renders no
// fabricated example block (the 14-probe matrix hit 7/8 without one).

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

	m := &Manifest{tools: selected}
	for _, t := range selected {
		m.Declared = append(m.Declared, t.Name)
	}

	m.instructions = instructions(c, parallelOff)

	// The preamble is fully assembled in header(): the hermes-compact skeleton
	// carries the definitions inline (that ordering is what the 14-probe
	// wording matrix measured, probe/outP1..C). Every tool renders its full
	// definition regardless of how many the client declared.
	m.Preamble = m.header()
	return m, nil
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
