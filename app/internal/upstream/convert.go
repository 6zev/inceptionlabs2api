// Package upstream talks to chat.inceptionlabs.ai: it acquires anonymous
// session tokens, converts OpenAI messages to the AI SDK parts format,
// issues /api/chat requests, and reads the upstream SSE stream.
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"inceptionlabs2api/internal/openai"
)

// Part is one entry of an upstream message's parts array.
type Part struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	State string `json:"state,omitempty"`
}

// Message is one upstream chat message in the AI SDK parts format.
type Message struct {
	Parts []Part `json:"parts"`
	ID    string `json:"id"`
	Role  string `json:"role"`
}

// ChatRequest is the POST /api/chat body.
type ChatRequest struct {
	ReasoningEffort  string    `json:"reasoningEffort"`
	WebSearchEnabled bool      `json:"webSearchEnabled"`
	VoiceMode        bool      `json:"voiceMode"`
	Timezone         string    `json:"timezone"`
	Language         string    `json:"language"`
	ID               string    `json:"id"`
	Messages         []Message `json:"messages"`
	Trigger          string    `json:"trigger"`
}

// NewChatRequest builds the upstream request with the fixed web-client
// fields the audit captured.
func NewChatRequest(msgs []openai.TextMessage, effort string) *ChatRequest {
	converted := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		parts := make([]Part, 0, len(m.Texts))
		for _, text := range m.Texts {
			p := Part{Type: "text", Text: text}
			if m.Role == "assistant" {
				p.State = "done"
			}
			parts = append(parts, p)
		}
		converted = append(converted, Message{Parts: parts, ID: RandomID("m"), Role: m.Role})
	}
	if effort == "" {
		effort = "medium"
	}
	return &ChatRequest{
		ReasoningEffort:  effort,
		WebSearchEnabled: false,
		VoiceMode:        false,
		Timezone:         "UTC",
		Language:         "en-US",
		ID:               RandomID("c"),
		Messages:         converted,
		Trigger:          "submit-message",
	}
}

// RandomID returns "<prefix>-<12 hex chars>". If the system entropy source
// fails it falls back to a fixed suffix rather than panicking; ids are
// cosmetic and never used for security.
func RandomID(prefix string) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%s-000000000000", prefix)
	}
	return prefix + "-" + hex.EncodeToString(b)
}
