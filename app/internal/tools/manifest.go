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

	tools []Tool
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

	m := &Manifest{Tiered: len(selected) > TierThreshold, tools: selected}
	for _, t := range selected {
		m.Declared = append(m.Declared, t.Name)
	}

	var b strings.Builder
	b.WriteString("# Tool calling\n\n")
	b.WriteString("You can call the functions listed below. To call a function, emit exactly:\n\n")
	b.WriteString(callStart + "\n")
	b.WriteString(`{"name": "function_name", "arguments": {"arg": "value"}}` + "\n")
	b.WriteString(callEnd + "\n\n")
	b.WriteString("Emit one block per call, with a complete JSON object. Do not wrap the block in a code fence. ")
	b.WriteString("Only use the function names below; never invent one. If no function is needed, answer normally.\n")

	switch {
	case c.Named != "":
		fmt.Fprintf(&b, "\nYou must call the function %q. Do not call any other function.\n", c.Named)
	case c.Required:
		b.WriteString("\nYou MUST call at least one function before answering.\n")
	}
	if parallelOff {
		b.WriteString("\nCall at most one function per turn.\n")
	}

	if m.Tiered {
		m.Loader = true
		fmt.Fprintf(&b, "\nParameter schemas are omitted to save context. Before calling a function, if you do not know its parameters, call `%s` once with {\"names\": [\"…\"]} or {\"query\": \"…\"} to load them.\n", toolSchemaName)
		b.WriteString("\n<tools>\n")
		for _, t := range selected {
			fmt.Fprintf(&b, `{"name":%s,"description":%s}`+"\n", quoteJSON(t.Name), quoteJSON(t.Description))
		}
		b.WriteString("</tools>\n")
		// Declare the loader last so it never shadows a real tool's name.
		m.Declared = append(m.Declared, toolSchemaName)
	} else {
		b.WriteString("\n<tools>\n")
		for _, t := range selected {
			fmt.Fprintf(&b, `{"name":%s,"description":%s,"parameters":%s}`+"\n",
				quoteJSON(t.Name), quoteJSON(t.Description), compactParams(t.Parameters))
		}
		b.WriteString("</tools>\n")
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
