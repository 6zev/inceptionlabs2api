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
//
// The capability fields exist so a client can tell what the model can do
// without guessing: without them a UI has no way to know that thinking is
// available at all, and therefore never sends reasoning_effort nor shows the
// reasoning_content the gateway already forwards.
//
// Two shapes are published side by side, because no single schema is standard:
//
//   - capabilities / supports_reasoning / reasoning_levels / limits, the
//     gateway's own vocabulary, and
//   - the OpenRouter shape (context_length, architecture, supported_parameters,
//     pricing, top_provider). Real clients parse only this one: Chatbox reads
//     context_length and derives the reasoning capability from architecture +
//     supported_parameters (src/shared/models/openai-compatible.ts), and a
//     second vocabulary would have to be invented to say the same thing.
//
// Context is reported in tokens here because the model card publishes a token
// window. The transport caps that this deployment really enforces — 100
// messages, ~4 MB body — are reported separately in limits, as what they are.
// No pricing is advertised beyond zeros: the gateway does not bill.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	// Capabilities is the nested form LiteLLM/OpenRouter-style clients read.
	Capabilities ModelCapabilities `json:"capabilities"`
	// The flat duplicates below cost nothing and help clients that look for a
	// top-level key instead of the nested object. No model object schema
	// standardises these, so more than one shape is offered.
	SupportsReasoning bool     `json:"supports_reasoning"`
	ReasoningLevels   []string `json:"reasoning_levels"`
	// Limits reports the real upstream boundaries, named for what they are
	// rather than pretending to be a token window.
	Limits ModelLimits `json:"limits"`

	// OpenRouter-shaped metadata. See the type comment.
	ContextLength       int               `json:"context_length"`
	Architecture        ModelArchitecture `json:"architecture"`
	SupportedParameters []string          `json:"supported_parameters"`
	Pricing             ModelPricing      `json:"pricing"`
	TopProvider         ModelTopProvider  `json:"top_provider"`
}

// ModelArchitecture is OpenRouter's modality block. Chatbox only builds its
// capabilities list when this object is present, so it is not optional even
// though the backend is text-in/text-out.
type ModelArchitecture struct {
	Modality         string   `json:"modality"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
	Tokenizer        string   `json:"tokenizer"`
	// InstructType is null for models without a published instruction format,
	// matching OpenRouter.
	InstructType *string `json:"instruct_type"`
}

// ModelPricing mirrors OpenRouter's string-typed price fields. Every value is
// "0": the gateway is free and bills nothing, and a client that multiplies
// these gets zero rather than a guess.
type ModelPricing struct {
	Prompt            string `json:"prompt"`
	Completion        string `json:"completion"`
	Request           string `json:"request"`
	Image             string `json:"image"`
	WebSearch         string `json:"web_search"`
	InternalReasoning string `json:"internal_reasoning"`
}

// ModelTopProvider is the serving provider's own limits, in the same shape
// OpenRouter uses, so a client that reads only that block still sees the
// window and the output cap.
type ModelTopProvider struct {
	ContextLength       int  `json:"context_length"`
	MaxCompletionTokens int  `json:"max_completion_tokens"`
	IsModerated         bool `json:"is_moderated"`
}

// ModelCapabilities are what the backend can do.
type ModelCapabilities struct {
	Streaming bool `json:"streaming"`
	Tools     bool `json:"tools"`
	// Reasoning is true because reasoning is available at effort >= low. It is
	// not true of every request: reasoning_effort=instant produces no
	// reasoning at all (measured 2026-10-26, 0/4 samples), so a client must not
	// assume reasoning_content is always present.
	Reasoning bool `json:"reasoning"`
	// WebSearch is the gateway-served tool, so it follows configuration rather
	// than the backend.
	WebSearch bool `json:"web_search"`
}

// ModelLimits are the measured upstream request boundaries. These are message
// and byte counts, not tokens: the upstream publishes no token accounting, so
// there is nothing truer to report.
type ModelLimits struct {
	MaxMessages int `json:"max_messages"`
	// MaxInputBytes is the observed ~4 MB body ceiling.
	MaxInputBytes int64 `json:"max_input_bytes"`
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
