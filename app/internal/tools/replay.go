package tools

import (
	"crypto/rand"
	"encoding/json"
	"strings"
)

// NewID mints an OpenAI-shaped tool-call id: "call_" plus 24 alphanumerics.
// The format is convention, not spec (the OpenAI schema constrains neither
// prefix nor length), but it is what every real client sees, so it is what a
// client-side validator is likely to accept. Ids are cosmetic and never used
// for security; on entropy failure a fixed suffix is returned rather than
// panicking, matching upstream.RandomID.
func NewID() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "call_000000000000000000000000"
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "call_" + string(b)
}

// EncodedCall is one assistant tool call as it appears in a replayed history.
type EncodedCall struct {
	ID        string
	Name      string
	Arguments string // JSON-encoded string exactly as the client sent it
}

// RenderAssistant renders an assistant turn's text plus its calls as the
// upstream should see it. The block form is the same one the preamble teaches,
// with the call id added so a following tool result can be associated.
func RenderAssistant(text string, calls []EncodedCall) string {
	if len(calls) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(text)
	for _, c := range calls {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteByte('\n')
		}
		b.WriteString(callStart)
		b.WriteString(`{"id":`)
		b.WriteString(quoteJSON(c.ID))
		b.WriteString(`,"name":`)
		b.WriteString(quoteJSON(c.Name))
		b.WriteString(`,"arguments":`)
		b.WriteString(argumentsAsJSON(c.Arguments))
		b.WriteByte('}')
		b.WriteString(callEnd)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// argumentsAsJSON embeds a client-supplied arguments string as a JSON value
// when it is an object, and as a JSON string otherwise. The replayed block is
// therefore always syntactically valid even when the client echoed garbage.
func argumentsAsJSON(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed != "" && strings.HasPrefix(trimmed, "{") {
		var v map[string]json.RawMessage
		if json.Unmarshal([]byte(trimmed), &v) == nil {
			return trimmed
		}
	}
	return quoteJSON(raw)
}

// Result is one tool result as the client supplied it.
type Result struct {
	CallID  string
	Name    string
	Content string
}

// resultContract is the trailer line of a rendered result turn. It exists to
// break the narrate-no-call stall: after consuming a tool result the model
// sometimes ends its turn with a plan narration and no call block — a
// self-reinforcing pattern once one such turn is in history. The trailer
// states the continuation rule at the exact point the decision is made.
const resultContract = "[End of tool result. If the task is not complete, call the next tool now — narration alone is not a valid turn.]"

// RenderResults renders one or more tool results as a single user turn. The
// upstream has no tool role; results ride in a user message. The format is a
// single-layer bracket block, deliberately NOT XML: client tool results are
// frequently XML documents themselves (`<tool_result>…</tool_result>` from
// agent frameworks), and the old XML envelope wrapped that content into
// nested markup the model read as structure. A bracket header no result body
// can forge closes each block; the contract trailer appears once per turn,
// after the last result.
func RenderResults(results []Result) string {
	var b strings.Builder
	for i, r := range results {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("[Tool Result")
		if r.CallID != "" {
			b.WriteString(` id="`)
			b.WriteString(attrEscape(r.CallID))
			b.WriteByte('"')
		}
		if r.Name != "" {
			b.WriteString(` name="`)
			b.WriteString(attrEscape(r.Name))
			b.WriteByte('"')
		}
		b.WriteString("]\n")
		b.WriteString(r.Content)
		b.WriteByte('\n')
		if i == len(results)-1 {
			b.WriteString(resultContract)
		} else {
			b.WriteString("[End of tool result]")
		}
	}
	return b.String()
}

// attrEscape makes a value safe inside a double-quoted bracket attribute: it
// removes the characters that could terminate the attribute or forge a new
// line. XML entity escaping is intentionally NOT applied — the block is not
// XML and entities would reach the model literally.
func attrEscape(s string) string {
	r := strings.NewReplacer(
		`"`, `'`,
		"]", "",
		"\n", " ",
		"\r", " ",
	)
	return r.Replace(s)
}
