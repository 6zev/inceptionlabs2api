package openai

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// History is one message of a tool-aware conversion: the already-normalised
// role and texts, plus the tool payload the converter needs.
type History struct {
	Role  string
	Texts []string

	// Calls is the assistant turn's tool_calls, in wire order. Empty for any
	// other role.
	Calls []ToolCall
	// Results is a tool turn's result. Role "tool" only.
	Results []ToolResult
}

// ToolResult is one role:"tool" message reduced to what the replay encoder
// needs. An assistant turn may be answered by several consecutive tool
// messages; each is one result.
type ToolResult struct {
	CallID  string
	Content string
}

// NormalizeHistory is NormalizeMessages plus tool awareness. It behaves
// identically to NormalizeMessages for every message without tool payloads.
//
// Rules (SPEC §4.1):
//   - An assistant message with a malformed or empty tool_calls array keeps
//     only its text; the array is dropped, not rejected.
//   - A role:"tool" message keeps its tool_call_id when it is a JSON string.
//     It is a result even when its content is empty (an empty result is a
//     legitimate answer: "{}"), so it is the one message type that may have no
//     text without being a 400.
//   - Content of a tool message is flattened exactly like any other content.
func NormalizeHistory(msgs []Message) ([]History, int, *RequestError) {
	out := make([]History, 0, len(msgs))
	total := 0

	for i, m := range msgs {
		texts := contentTexts(m.Content)
		role := normalizeRole(m.Role)

		if m.Role == "tool" {
			// A tool result with no content is still a result; the upstream
			// gets an empty envelope rather than a dropped message.
			total += runeCount(texts)
			out = append(out, History{Role: "user", Results: []ToolResult{{
				CallID:  rawString(m.ToolCallID),
				Content: strings.Join(texts, ""),
			}}})
			continue
		}

		// An assistant turn may be nothing but tool_calls: that is a complete
		// message, and OpenAI's schema permits content:null for it.
		calls := parseToolCalls(m.ToolCalls)
		if len(texts) == 0 && len(calls) == 0 {
			return nil, 0, &RequestError{
				Status:  400,
				Type:    "invalid_request_error",
				Code:    CodeInvalidRequest,
				Param:   "messages[" + itoa(i) + "].content",
				Message: "messages[" + itoa(i) + "] (" + m.Role + ") has no text content",
			}
		}
		total += runeCount(texts)

		h := History{Role: role, Texts: texts}
		if m.Role == "assistant" {
			h.Calls = calls
		}
		out = append(out, h)
	}
	return out, total, nil
}

// parseToolCalls decodes an assistant tool_calls array, dropping entries that
// lack an id or a function name. A malformed array yields nil: the text alone
// is replayed.
func parseToolCalls(raw json.RawMessage) []ToolCall {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var calls []ToolCall
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil
	}
	kept := calls[:0]
	for _, c := range calls {
		if c.ID == "" || c.Function.Name == "" {
			continue
		}
		if c.Type == "" {
			c.Type = "function"
		}
		kept = append(kept, c)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// rawString reads an optional JSON string, returning "" for null, absent or
// any other type.
func rawString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func runeCount(texts []string) int {
	n := 0
	for _, t := range texts {
		n += utf8.RuneCountInString(t)
	}
	return n
}

// itoa is small enough not to pull strconv in for one call site.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
