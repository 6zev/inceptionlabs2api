package server

import (
	"encoding/json"

	"inceptionlabs2api/internal/openai"
)

// trimMessages caps the message count because the upstream web chat rejects any
// conversation with more than 100 messages:
//
//	{"error":"This conversation is too long. Please start a new chat to continue."}
//
// Measured (2026-10-22): 100 messages pass, 101 fail, independent of payload
// size (a single 900 KB message is accepted), so this is a message-count cap,
// not a token cap.
//
// This is the last line of defence, not the primary strategy: openai.FoldHistory
// already compresses history into one transcript bubble well before this fires.
// Folding is lossless-but-compressing; trimming is lossy, and only runs when the
// folded history still exceeds the cap (a pathological tail of huge recent
// turns, where the fold is forbidden to touch the hot window). Order in
// chatCompletions is therefore: fold, then trim.
//
// It keeps every leading system/developer message plus the newest turns, and
// drops old turns in whole groups so an assistant tool_calls message is never
// separated from the tool results that answer it. It returns the number of
// dropped messages so the caller can log it.
func trimMessages(msgs []openai.Message, max int) ([]openai.Message, int) {
	if max <= 0 || len(msgs) <= max {
		return msgs, 0
	}

	head := 0
	for head < len(msgs) && isSystemRole(msgs[head].Role) {
		head++
	}
	if head > max { // pathological: the system preamble alone is over budget
		head = max
	}

	// Group the tail: a message that carries tool_calls owns the tool results
	// that follow it, so the whole span is kept or dropped together.
	type span struct{ start, end int } // [start,end)
	var groups []span
	for i := head; i < len(msgs); {
		j := i + 1
		if hasToolCalls(msgs[i]) {
			for j < len(msgs) && msgs[j].Role == "tool" {
				j++
			}
		}
		groups = append(groups, span{i, j})
		i = j
	}
	if len(groups) == 0 {
		return msgs, 0
	}

	// Fill the budget from the newest group backwards.
	budget := max - head
	used := 0
	keepIdx := len(groups)
	for i := len(groups) - 1; i >= 0; i-- {
		size := groups[i].end - groups[i].start
		if used+size > budget {
			break
		}
		used += size
		keepIdx = i
	}
	if keepIdx >= len(groups) {
		// Even the newest group alone does not fit: keep just that group,
		// accepting that the result may exceed max. Dropping the newest turn
		// would answer a question with no question in it.
		keepIdx = len(groups) - 1
	}

	keepFrom := groups[keepIdx].start
	out := make([]openai.Message, 0, head+len(msgs)-keepFrom)
	out = append(out, msgs[:head]...)
	out = append(out, msgs[keepFrom:]...)

	// Drop leading non-user turns left over from the cut so the upstream sees
	// a prompt first, then a normal alternating conversation. Never leave an
	// orphaned tool result behind: a role:"tool" message whose assistant
	// tool_calls turn was just stripped is invalid history, so it goes with it.
	for len(out) > head && out[head].Role != "user" {
		if out[head].Role == "tool" {
			// The orphan and any remaining consecutive results belong to the
			// same broken group; drop the whole run.
			for len(out) > head && out[head].Role == "tool" {
				out = append(out[:head], out[head+1:]...)
			}
			continue
		}
		out = append(out[:head], out[head+1:]...)
	}
	return out, len(msgs) - len(out)
}

// isSystemRole reports whether role is a leading-instruction turn.
func isSystemRole(role string) bool {
	return role == "system" || role == "developer"
}

// hasToolCalls reports whether an assistant message carries a tool_calls
// payload. The field is raw JSON ("" or "null" when absent).
func hasToolCalls(m openai.Message) bool {
	raw := m.ToolCalls
	if len(raw) == 0 {
		return false
	}
	s := string(raw)
	if s == "null" || s == "[]" {
		return false
	}
	var calls []json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil {
		return false
	}
	return len(calls) > 0
}
