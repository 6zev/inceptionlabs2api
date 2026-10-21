package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// supportedParams are the only top-level request fields the gateway accepts.
// Every other key is rejected with a message naming it, so clients are never
// silently ignored.
var supportedParams = []string{"model", "messages", "stream", "reasoning_effort"}

// toolParams are recognised OpenAI fields that the gateway deliberately does
// not implement. They get a dedicated error instead of a generic one.
var toolParams = map[string]bool{
	"tools":         true,
	"tool_choice":   true,
	"functions":     true,
	"function_call": true,
}

// reasoningEfforts is the accepted enum for reasoning_effort.
var reasoningEfforts = map[string]string{
	"low":    "low",
	"medium": "medium",
	"high":   "high",
	// OpenAI also sends "minimal" for some models; Mercury has no equivalent,
	// so it maps down to low rather than failing.
	"minimal": "low",
}

// Decode parses and validates an OpenAI chat request body. Unknown top-level
// keys, unsupported roles, media content, tool calling, and malformed values
// are rejected with a *RequestError.
func Decode(body []byte) (*ChatRequest, *RequestError) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&raw); err != nil {
		return nil, Invalid(fmt.Sprintf("invalid JSON body: %v", err))
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err == nil {
		return nil, Invalid("invalid JSON body: unexpected data after the request object")
	}
	if raw == nil {
		return nil, Invalid("request body must be a JSON object")
	}

	var unknown, tools []string
	for key := range raw {
		if toolParams[key] {
			tools = append(tools, key)
			continue
		}
		if !isSupported(key) {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	sort.Strings(tools)
	if len(tools) > 0 {
		return nil, &RequestError{
			Status:  400,
			Type:    "invalid_request_error",
			Code:    CodeToolUnsupported,
			Param:   tools[0],
			Message: fmt.Sprintf("tool calling is not supported by this gateway; remove %s", quoteList(tools)),
		}
	}
	if len(unknown) > 0 {
		return nil, &RequestError{
			Status: 400,
			Type:   "invalid_request_error",
			Code:   CodeUnsupportedParam,
			Param:  unknown[0],
			Message: fmt.Sprintf("unsupported parameter(s): %s; this gateway accepts only: %s",
				quoteList(unknown), quoteList(supportedParams)),
		}
	}

	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, Invalid(fmt.Sprintf("invalid request: %v", err))
	}

	if req.Model == "" {
		return nil, Invalid("model is required")
	}
	if len(req.Messages) == 0 {
		return nil, Invalid("messages must not be empty")
	}
	if req.ReasoningEffort != "" {
		effort, ok := reasoningEfforts[req.ReasoningEffort]
		if !ok {
			return nil, &RequestError{
				Status:  400,
				Type:    "invalid_request_error",
				Code:    CodeInvalidRequest,
				Param:   "reasoning_effort",
				Message: fmt.Sprintf("reasoning_effort must be one of low, medium, high; got %q", req.ReasoningEffort),
			}
		}
		req.ReasoningEffort = effort
	} else {
		req.ReasoningEffort = "medium"
	}
	return &req, nil
}

func isSupported(key string) bool {
	for _, k := range supportedParams {
		if k == key {
			return true
		}
	}
	return false
}

// quoteList renders ["a","b"] as `"a", "b"`.
func quoteList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = fmt.Sprintf("%q", item)
	}
	return strings.Join(quoted, ", ")
}
