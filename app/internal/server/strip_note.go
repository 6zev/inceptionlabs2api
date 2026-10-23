package server

import (
	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/tools"
)

// stripSwitchNotes removes a leading model-switch note ("[Note: model was
// just switched ... Adjust your self-identification accordingly.]") from
// user-role texts of one decoded request, in place. Called once at the
// chat-completions door before any path decision, so legacy, tools and
// native paths all see clean user text and the measured identity refusal
// (il2a-live2 A/B: the note is the sole trigger) never fires; the
// retry-path scrub stays as a belt for other refusal shapes.
//
// Scope: user-role messages only, leading "[Note: ...]" blocks only (the
// measured reflex shape — a note quoted mid-conversation is left alone),
// this request's decoded representation only — the client's own stored
// history is never touched. System, assistant and tool messages are never
// modified; a note inside them is not the measured trigger.
func stripSwitchNotes(req *openai.ChatRequest) {
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role != "user" {
			continue
		}
		switch c := m.Content.(type) {
		case string:
			m.Content = tools.StripSwitchNote(c)
		case []any:
			for _, item := range c {
				part, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if typ, _ := part["type"].(string); typ != "text" {
					continue
				}
				if text, ok := part["text"].(string); ok {
					part["text"] = tools.StripSwitchNote(text)
				}
			}
		}
	}
}
