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
	// Additions after the measured skeleton (wire-format law: the skeleton
	// wording is protocol and stays byte-identical; these lines are added
	// mechanisms only) — a concrete worked example of the marker block and
	// the continuation rule that breaks the narrate-no-call stall.
	if ex := workedExample(m.tools); ex != "" {
		preamble += ex + "\n"
	}
	preamble += continuationRuleSentence + "\n"
	return preamble + m.instructions
}

// continuationRuleSentence is the anti-stall rule, appended to every preamble
// style. It targets the narrate-no-call stall: after consuming a tool result
// the model sometimes ends its turn with a plan narration and zero call
// blocks — a self-reinforcing pattern once one such turn is in history.
const continuationRuleSentence = "After a tool result arrives, if the task is not yet complete, immediately call the next tool. Never end your turn with only a description of what you are about to do."

// workedExample renders concrete marker-block examples from the declared
// tools' real names and minimal argument shapes derived from their parameter
// schemas, one "Example:" line per tool for up to TWO tools — parity with the
// reference implementations (ToolForge xyml.py _example_input_from_tool,
// toolkit render.go). A concrete example of the exact wire shape is cheaper
// for the model to imitate than prose alone. Blank-named tools are skipped
// (the next named tool takes the slot) and a name is never rendered twice.
// No tools at all skips the examples entirely.
func workedExample(list []Tool) string {
	var b strings.Builder
	seen := make(map[string]bool, 2)
	for _, t := range list {
		if strings.TrimSpace(t.Name) == "" || seen[t.Name] {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		seen[t.Name] = true
		b.WriteString("Example: " + callStart + `{"name":` + quoteJSON(t.Name) +
			`,"arguments":` + exampleArguments(t.Parameters) + "}" + callEnd)
		if len(seen) == 2 {
			break
		}
	}
	return b.String()
}

// exampleArguments derives a minimal argument object from a JSON schema.
// When the schema lists required properties only those are rendered;
// otherwise every property renders, in schema order so the example mirrors
// the definition line the model just read. Type mapping: string -> "...",
// integer/number -> 1, boolean -> true, object -> {}, array -> [],
// anything else -> "". An absent or unparseable schema renders {}.
func exampleArguments(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "{}"
	}
	names, props, required, ok := schemaProperties([]byte(trimmed))
	if !ok {
		return "{}"
	}
	if len(required) == 0 {
		required = names
	}
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for _, name := range required {
		prop, ok := props[name]
		if !ok {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(quoteJSON(name))
		b.WriteByte(':')
		b.WriteString(exampleValue(prop))
	}
	b.WriteByte('}')
	return b.String()
}

// schemaProperties walks a schema's properties object in document order and
// its required list. ok is false when raw is not a parseable schema object.
func schemaProperties(raw []byte) (names []string, props map[string]json.RawMessage, required []string, ok bool) {
	props = make(map[string]json.RawMessage)
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, nil, nil, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, nil, false
		}
		key, _ := keyTok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, nil, nil, false
		}
		switch key {
		case "properties":
			names, ok = objectKeys(value)
			if !ok {
				return nil, nil, nil, false
			}
			if err := json.Unmarshal(value, &props); err != nil {
				return nil, nil, nil, false
			}
		case "required":
			if err := json.Unmarshal(value, &required); err != nil {
				required = nil
			}
		}
	}
	return names, props, required, true
}

// objectKeys returns the keys of a JSON object in document order.
func objectKeys(raw json.RawMessage) ([]string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var keys []string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, false
		}
		keys = append(keys, key)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, false
		}
	}
	return keys, true
}

// exampleValue renders the minimal placeholder for one schema property.
// JSON Schema allows "type" to be either a single name or an array of names
// (e.g. ["string","null"] for a nullable string); an array picks the example
// from the first non-"null" entry, with the concrete-type precedence
// array > object > boolean > integer/number > string when several concrete
// types are listed (mirroring ToolForge's _example_value).
func exampleValue(prop json.RawMessage) string {
	var p struct {
		Type json.RawMessage `json:"type"`
	}
	if err := json.Unmarshal(prop, &p); err != nil {
		return `"..."` // unparseable property: a string is the safest shape
	}
	return exampleForType(p.Type)
}

// exampleForType maps a schema "type" value — single name, array of names, or
// absent — to the minimal example placeholder.
func exampleForType(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return `"..."` // absent type: a string is the safest shape
	}
	var names []string
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(raw, &names); err != nil {
			return `"..."`
		}
	} else {
		var single string
		if err := json.Unmarshal(raw, &single); err != nil {
			return `"..."`
		}
		names = []string{single}
	}
	if len(names) == 0 {
		return `"..."`
	}
	// First non-"null" entry; when several concrete types remain, the
	// precedence order below decides (array > object > boolean > number >
	// string), not list position.
	var pick string
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if n == "null" {
			continue
		}
		if !set[n] {
			set[n] = true
			if pick == "" {
				pick = n
			}
		}
	}
	for _, preferred := range []string{"array", "object", "boolean", "integer", "number", "string"} {
		if set[preferred] {
			return exampleForName(preferred)
		}
	}
	if pick != "" {
		return `"..."` // a concrete type we do not model: safest shape
	}
	return `"..."` // all-null type array
}

// exampleForName renders the placeholder for one concrete schema type name.
func exampleForName(name string) string {
	switch name {
	case "string":
		return `"..."`
	case "integer", "number":
		return "1"
	case "boolean":
		return "true"
	case "object":
		return "{}"
	case "array":
		return "[]"
	}
	return `"..."`
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
