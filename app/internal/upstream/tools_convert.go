package upstream

import (
	"strings"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/tools"
)

// NewToolChatRequestHistory builds the upstream body from tool-aware history.
//
// Placement follows the Qwen/Hermes templates: the tool preamble is a system
// message before everything else, the assistant's calls replay as
// <tool_call> blocks in its own turn, and consecutive role:"tool" results are
// grouped into one user turn (DESIGN-TOOLS.md §1, §4).
func NewToolChatRequestHistory(hist []openai.History, effort string, m *tools.Manifest) *ChatRequest {
	converted := make([]Message, 0, len(hist)+1)

	if m != nil && m.Preamble != "" {
		converted = append(converted, Message{
			Parts: []Part{{Type: "text", Text: m.Preamble}},
			ID:    RandomID("m"),
			Role:  "system",
		})
	}

	// names maps a call id to its function name so a tool result can be
	// labelled; the OpenAI tool message itself carries no name.
	names := make(map[string]string)
	for _, h := range hist {
		for _, c := range h.Calls {
			names[c.ID] = c.Function.Name
		}
	}

	for i := 0; i < len(hist); i++ {
		h := hist[i]

		if len(h.Results) > 0 {
			// Group this run of consecutive results into one user turn.
			var results []tools.Result
			for ; i < len(hist) && len(hist[i].Results) > 0; i++ {
				for _, r := range hist[i].Results {
					results = append(results, tools.Result{
						CallID:  r.CallID,
						Name:    names[r.CallID],
						Content: r.Content,
					})
				}
			}
			i--
			texts := []string{tools.RenderResults(results)}
			converted = append(converted, Message{
				Parts: textParts("user", texts),
				ID:    RandomID("m"),
				Role:  "user",
			})
			continue
		}

		var texts []string
		if len(h.Calls) > 0 {
			var calls []tools.EncodedCall
			for _, c := range h.Calls {
				calls = append(calls, tools.EncodedCall{ID: c.ID, Name: c.Function.Name, Arguments: c.Function.Arguments})
			}
			rendered := tools.RenderAssistant(strings.Join(h.Texts, ""), calls)
			if rendered != "" {
				texts = []string{rendered}
			}
		} else {
			texts = h.Texts
		}
		if len(texts) == 0 {
			continue
		}
		converted = append(converted, Message{
			Parts: textParts(h.Role, texts),
			ID:    RandomID("m"),
			Role:  h.Role,
		})
	}

	if effort == "" {
		effort = "medium"
	}
	// Web search is disabled whenever a tool manifest is present. Measured
	// 2026-10-22 (probe/outQ): with webSearchEnabled:true the upstream injects
	// its own server-side web-search tool definitions, and preamble-aside those
	// two catalogues conflict — 3/3 empty answers regardless of reasoning
	// effort (medium/instant/high), while the identical preamble with
	// webSearchEnabled:false answered with a clean <tool_call> block. A client that
	// declares tools wants ITS tools honored; tool-less chat keeps search on.
	//
	// This also holds for the gateway's native web_search tool: it is served
	// from OUR manifest (one catalogue) and must never be paired with the
	// upstream's builtin search.
	return &ChatRequest{
		ReasoningEffort:  effort,
		WebSearchEnabled: m == nil,
		VoiceMode:        false,
		Timezone:         "UTC",
		Language:         "en-US",
		ID:               RandomID("c"),
		Messages:         converted,
		Trigger:          "submit-message",
	}
}

// textParts renders texts with the state rule: assistant parts are "done".
func textParts(role string, texts []string) []Part {
	parts := make([]Part, 0, len(texts))
	for _, text := range texts {
		p := Part{Type: "text", Text: text}
		if role == "assistant" {
			p.State = "done"
		}
		parts = append(parts, p)
	}
	return parts
}
