// Package openai defines the gateway's OpenAI Chat Completions wire types,
// request validation, and error envelope.
package openai

import "encoding/json"

// ChatRequest is the accepted subset of an OpenAI chat completion request.
// Every other top-level field is rejected before decoding reaches this type.
type ChatRequest struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Stream          bool      `json:"stream"`
	ReasoningEffort string    `json:"reasoning_effort"`
}

// Message is one OpenAI chat message. Content is either a string or an array
// of content parts; it is kept raw so the decoder can reject media parts with
// a precise error.
type Message struct {
	Role       string          `json:"role"`
	Content    any             `json:"content"`
	Name       string          `json:"name"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

// ContentPart is a single entry of a multi-part message content array.
type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Model is one entry of GET /v1/models.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelList is the GET /v1/models envelope.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// Chunk is one streaming frame (object "chat.completion.chunk").
type Chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
}

// ChunkChoice is one choice in a streaming frame.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
	Logprobs     *any    `json:"logprobs"`
}

// Delta is the incremental payload of a streaming frame. Empty fields are
// omitted.
type Delta struct {
	Role             string `json:"role,omitempty"`
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// Completion is a non-streaming response (object "chat.completion").
type Completion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
}

// CompletionChoice is one choice in a non-streaming response.
type CompletionChoice struct {
	Index        int               `json:"index"`
	Message      CompletionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
	Logprobs     *any              `json:"logprobs"`
}

// CompletionMessage is the aggregated assistant message. Usage is
// intentionally absent: the upstream publishes no token counts and the
// gateway does not fabricate them.
type CompletionMessage struct {
	Role             string `json:"role"`
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ErrorResponse is OpenAI's error envelope.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the body of an error response.
type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}
