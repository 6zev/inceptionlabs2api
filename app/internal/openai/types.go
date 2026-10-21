// Package openai defines the gateway's OpenAI Chat Completions wire types,
// request validation, and error envelope.
package openai

import "encoding/json"

// ChatRequest is the subset of an OpenAI chat completion request that maps
// onto the upstream, plus the tool fields (§4.1). Unknown top-level fields
// (temperature, response format, ...) are intentionally absent: they are
// dropped during decoding, not rejected (SPEC §1.1).
type ChatRequest struct {
	Model           string    `json:"model"`
	Messages        []Message `json:"messages"`
	Stream          bool      `json:"stream"`
	ReasoningEffort string    `json:"reasoning_effort"`

	Tools             []Tool     `json:"tools"`
	ToolChoice        ToolChoice `json:"-"`
	ParallelToolCalls *bool      `json:"parallel_tool_calls"`
}

// Tool is one OpenAI function tool.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction is the callable half of a Tool. Parameters is kept raw: an
// absent or unparseable schema renders as {} rather than rejecting the call.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolChoiceKind is the normalised tool_choice variant (§5 of
// DESIGN-TOOLS.md).
type ToolChoiceKind int

// The four OpenAI tool_choice variants. Auto is the zero value: OpenAI
// documents "auto" as the default when tools are present.
const (
	ToolChoiceAuto ToolChoiceKind = iota
	ToolChoiceNone
	ToolChoiceRequired
	ToolChoiceNamed
)

// ToolChoice is the normalised tool_choice. Name is set only for
// ToolChoiceNamed.
type ToolChoice struct {
	Kind ToolChoiceKind
	Name string
}

// ToolCall is one assistant tool call, on both the response and the replayed
// request side.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the function half of a ToolCall. Arguments is a JSON-encoded
// string exactly as it crosses the wire.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Message is one OpenAI chat message. Content is either a string or an array
// of content parts; it is kept raw so media parts can be rendered as text
// placeholders instead of being rejected. ToolCalls and ToolCallID are raw so
// a malformed history entry cannot fail the whole request.
type Message struct {
	Role         string          `json:"role"`
	Content      any             `json:"content"`
	Name         string          `json:"name"`
	ToolCalls    json.RawMessage `json:"tool_calls"`
	FunctionCall json.RawMessage `json:"function_call"`
	ToolCallID   json.RawMessage `json:"tool_call_id"`
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
	Role             string          `json:"role,omitempty"`
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Annotations      []Citation      `json:"annotations,omitempty"`
	ToolCalls        []ToolCallDelta `json:"tool_calls,omitempty"`
}

// ToolCallDelta is one streaming tool-call fragment. Index is the only field
// OpenAI's schema requires on every fragment; id, type and name are sent on
// the first fragment of each call and omitted afterwards.
type ToolCallDelta struct {
	Index    int               `json:"index"`
	ID       string            `json:"id,omitempty"`
	Type     string            `json:"type,omitempty"`
	Function FunctionCallDelta `json:"function"`
}

// FunctionCallDelta is the function half of a ToolCallDelta.
type FunctionCallDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// Citation is one OpenAI url_citation annotation. Index is the 1-based
// citation number the answer text refers to; StartIndex/EndIndex are character
// offsets into the answer and are 0 when the upstream does not provide them
// (it does not — SPEC §2.4).
type Citation struct {
	Type       string `json:"type"`
	Index      int    `json:"index"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
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
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	Annotations      []Citation `json:"annotations,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
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
