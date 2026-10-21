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
	return &req, nil
}
