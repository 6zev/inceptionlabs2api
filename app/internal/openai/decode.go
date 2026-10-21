package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// reasoningEfforts maps the one OpenAI value Mercury has no equivalent for.
// Every other value is passed through: the upstream owns its enum, and a
// gateway-invented 400 would be a policy the caller never agreed to (§1.1).
var reasoningEfforts = map[string]string{
	"minimal": "low",
}

// Decode parses an OpenAI chat request body. It rejects only the structural
// minimum the upstream wire format cannot express: malformed JSON, a body
// that is not an object, a missing model/messages, and wrong JSON types.
// Everything else — unknown params, tools, media, long inputs — is accepted;
// unknown top-level keys are dropped by ChatRequest's field set.
func Decode(body []byte) (*ChatRequest, *RequestError) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&raw); err != nil {
		return nil, Invalid(fmt.Sprintf("invalid JSON body: %v", err))
	}
	// Anything other than EOF after the object — valid JSON or garbage — is
	// rejected: a prefix that a front proxy or WAF might parse differently
	// must not be silently tolerated. The other JSON routes (web_search,
	// audio) use the same rule.
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, Invalid("invalid JSON body: unexpected data after the request object")
	}
	if raw == nil {
		return nil, Invalid("request body must be a JSON object")
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
		if mapped, ok := reasoningEfforts[req.ReasoningEffort]; ok {
			req.ReasoningEffort = mapped
		}
	} else {
		req.ReasoningEffort = "medium"
	}
	if re := decodeTools(raw, &req); re != nil {
		return nil, re
	}
	return &req, nil
}

// decodeTools validates the tool fields. It reads tool_choice from the raw
// body because the field is a union of a string and an object.
//
// Only the structural minimum is rejected (class 2 in SPEC §1.1): `tools` must
// be an array of objects carrying a non-empty function name. Everything else —
// an unknown tool type, parameters that are not a schema, an unknown
// tool_choice string — is coerced, because the upstream answers no field here
// and a 400 would be the gateway's invention.
func decodeTools(raw map[string]json.RawMessage, req *ChatRequest) *RequestError {
	if body, ok := raw["tools"]; ok {
		var items []json.RawMessage
		if err := json.Unmarshal(body, &items); err != nil || items == nil {
			return Invalid("tools must be an array of function objects")
		}
		for i, item := range items {
			var t Tool
			if err := json.Unmarshal(item, &t); err != nil {
				return Invalid(fmt.Sprintf("tools[%d] must be an object", i))
			}
			if t.Function.Name == "" {
				return Invalid(fmt.Sprintf("tools[%d].function.name is required", i))
			}
			req.Tools = append(req.Tools, t)
		}
	}

	req.ToolChoice = ToolChoice{Kind: ToolChoiceAuto}
	if body, ok := raw["tool_choice"]; ok {
		req.ToolChoice = parseToolChoice(body)
	}
	return nil
}

// parseToolChoice normalises the union. An unrecognised value is auto: the
// OpenAI default whenever tools are present.
func parseToolChoice(body json.RawMessage) ToolChoice {
	var s string
	if err := json.Unmarshal(body, &s); err == nil {
		switch s {
		case "none":
			return ToolChoice{Kind: ToolChoiceNone}
		case "required":
			return ToolChoice{Kind: ToolChoiceRequired}
		default:
			return ToolChoice{Kind: ToolChoiceAuto}
		}
	}
	var named struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(body, &named); err == nil && named.Function.Name != "" {
		return ToolChoice{Kind: ToolChoiceNamed, Name: named.Function.Name}
	}
	return ToolChoice{Kind: ToolChoiceAuto}
}
