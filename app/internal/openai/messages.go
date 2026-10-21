package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxTextParts caps the number of text parts across a request. Empty parts
// are bytes that never reach the character guard, so an unbounded count would
// let a caller forward an arbitrarily large body upstream for free.
const maxTextParts = 4096

// TextMessage is a validated chat message reduced to plain text parts.
type TextMessage struct {
	Role  string
	Texts []string
}

// NormalizeMessages validates every message and flattens it to text parts.
// It returns the normalized messages and the total number of characters.
// maxChars > 0 enforces the context guard; -1 disables it.
func NormalizeMessages(msgs []Message, maxChars int) ([]TextMessage, int, *RequestError) {
	out := make([]TextMessage, 0, len(msgs))
	total, parts := 0, 0

	for i, m := range msgs {
		if hasToolCalls(m.ToolCalls) {
			return nil, 0, toolUnsupported(fmt.Sprintf("messages[%d].tool_calls", i), "tool_calls")
		}
		if m.Role == "tool" || m.Role == "function" {
			return nil, 0, toolUnsupported(fmt.Sprintf("messages[%d].role", i), fmt.Sprintf("role %q", m.Role))
		}
		role, ok := normalizeRole(m.Role)
		if !ok {
			return nil, 0, &RequestError{
				Status:  400,
				Type:    "invalid_request_error",
				Code:    CodeInvalidRequest,
				Param:   fmt.Sprintf("messages[%d].role", i),
				Message: fmt.Sprintf("unsupported role %q in messages[%d]; supported roles are system, developer, user, assistant", m.Role, i),
			}
		}

		texts, re := contentTexts(m.Content, i)
		if re != nil {
			return nil, 0, re
		}
		parts += len(texts)
		if parts > maxTextParts {
			return nil, 0, &RequestError{
				Status:  400,
				Type:    "invalid_request_error",
				Code:    CodeInvalidRequest,
				Param:   fmt.Sprintf("messages[%d].content", i),
				Message: fmt.Sprintf("request has more than %d text parts; merge adjacent text parts and retry", maxTextParts),
			}
		}
		if len(texts) == 0 {
			return nil, 0, &RequestError{
				Status:  400,
				Type:    "invalid_request_error",
				Code:    CodeInvalidRequest,
				Param:   fmt.Sprintf("messages[%d].content", i),
				Message: fmt.Sprintf("messages[%d] (%s) has no text content", i, m.Role),
			}
		}
		for _, t := range texts {
			total += utf8.RuneCountInString(t)
		}
		out = append(out, TextMessage{Role: role, Texts: texts})
	}

	if maxChars > 0 && total > maxChars {
		return nil, total, &RequestError{
			Status:  400,
			Type:    "invalid_request_error",
			Code:    CodeContextLength,
			Param:   "messages",
			Message: fmt.Sprintf("input is %d characters, which exceeds the %d-character limit; shorten the conversation", total, maxChars),
		}
	}
	return out, total, nil
}

// hasToolCalls reports whether a raw tool_calls value carries anything. Null,
// empty arrays and empty objects are treated as absent: many clients send
// "tool_calls": [] on ordinary assistant history.
func hasToolCalls(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	switch trimmed {
	case "", "null", "[]", "{}":
		return false
	}
	return true
}

// normalizeRole maps OpenAI roles onto the three roles the upstream accepts.
func normalizeRole(role string) (string, bool) {
	switch role {
	case "system", "developer":
		return "system", true
	case "user":
		return "user", true
	case "assistant":
		return "assistant", true
	default:
		return "", false
	}
}

// contentTexts extracts the text parts of a message content value. Media and
// unknown parts are rejected explicitly rather than silently dropped.
func contentTexts(content any, msgIndex int) ([]string, *RequestError) {
	switch c := content.(type) {
	case nil:
		return nil, nil
	case string:
		if c == "" {
			return nil, nil
		}
		return []string{c}, nil
	case []any:
		texts := make([]string, 0, len(c))
		for j, item := range c {
			part, ok := item.(map[string]any)
			if !ok {
				return nil, &RequestError{
					Status:  400,
					Type:    "invalid_request_error",
					Code:    CodeInvalidRequest,
					Param:   fmt.Sprintf("messages[%d].content[%d]", msgIndex, j),
					Message: "content parts must be JSON objects",
				}
			}
			typ, _ := part["type"].(string)
			if typ != "text" {
				return nil, mediaUnsupported(msgIndex, j, typ)
			}
			text, ok := part["text"].(string)
			if !ok || text == "" {
				return nil, &RequestError{
					Status:  400,
					Type:    "invalid_request_error",
					Code:    CodeInvalidRequest,
					Param:   fmt.Sprintf("messages[%d].content[%d].text", msgIndex, j),
					Message: fmt.Sprintf("messages[%d].content[%d] is a text part without a non-empty string \"text\" field", msgIndex, j),
				}
			}
			texts = append(texts, text)
		}
		return texts, nil
	default:
		return nil, &RequestError{
			Status:  400,
			Type:    "invalid_request_error",
			Code:    CodeInvalidRequest,
			Param:   fmt.Sprintf("messages[%d].content", msgIndex),
			Message: fmt.Sprintf("messages[%d].content must be a string or an array of text parts", msgIndex),
		}
	}
}

func mediaUnsupported(msgIndex, partIndex int, typ string) *RequestError {
	if typ == "" {
		typ = "unknown"
	}
	return &RequestError{
		Status:  400,
		Type:    "invalid_request_error",
		Code:    CodeContentUnsupported,
		Param:   fmt.Sprintf("messages[%d].content[%d].type", msgIndex, partIndex),
		Message: fmt.Sprintf("content part type %q is not supported; this gateway is text-only (send a text part, or describe the media in text)", typ),
	}
}

func toolUnsupported(param, field string) *RequestError {
	return &RequestError{
		Status:  400,
		Type:    "invalid_request_error",
		Code:    CodeToolUnsupported,
		Param:   param,
		Message: fmt.Sprintf("%s is not supported by this gateway; tool calling is out of scope", field),
	}
}
