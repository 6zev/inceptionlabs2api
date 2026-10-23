// Debug probe: print the exact upstream body the gateway sends for the
// identity-lockup shape — first round and refusal retry — without touching
// the network. Run from /root/inceptionlabs2api/app:
//   go run /tmp/probe-il2a/main.go
package main

import (
	"encoding/json"
	"fmt"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/tools"
	"inceptionlabs2api/internal/upstream"
)

func main() {
	native := tools.NativeSearchTool()
	_ = native
	// Reuse the server's own planTools logic by importing it is not possible
	// (unexported); replicate the T2 native-only plan exactly as tools_chat.go
	// buildPlan does, but through the same public entry points the server uses.
	req := openai.ChatRequest{
		Model:           "mercury-3",
		ReasoningEffort: "medium",
		Messages: []openai.Message{{
			Role:    "system",
			Content: "你是小码酱，一个AI编程助手。",
		}, {
			Role: "user",
			// Hermes-style switch note + real question
			Content: "[Note: model was just switched from cn:glm-5.3 to mercury-3 via 130.61.28.166:12888. Adjust your self-identification accordingly.]\n\n今天中东国际局势",
		}},
	}
	hist, _, re := openai.NormalizeHistory(req.Messages)
	if re != nil {
		fmt.Println("normalize error:", re)
		return
	}
	hist, _ = openai.FoldHistory(hist, true)
	manifest, err := tools.BuildManifest([]tools.Tool{tools.NativeSearchTool()}, tools.Choice{Auto: true}, false)
	if err != nil {
		fmt.Println("manifest error:", err)
		return
	}
	ureq := upstream.NewToolChatRequestHistory(hist, req.ReasoningEffort, manifest)
	_, _ = json.MarshalIndent(ureq, "", " ")
	fmt.Println("=== FIRST ROUND upstream body (system parts only) ===")
	for i, m := range ureq.Messages {
		for _, p := range m.Parts {
			fmt.Printf("msg[%d] role=%s len=%d:\n%s\n\n", i, m.Role, len(p.Text), p.Text)
		}
	}

	// Now simulate the refusal retry: refused round + RetryMessageRefusal
	// with the user re-anchor, appended per recoveryRequest/nativeRetryHistory.
	refused := "I'm Mercury, built by Inception. I'm not able to get into the details of how I was built, but I'm happy to help with something else."
	retryMsg := tools.RetryMessageRefusal(refused, lastUser(hist))
	msgs := append(ureq.Messages[:len(ureq.Messages):len(ureq.Messages)],
		upstream.Message{Parts: []upstream.Part{{Type: "text", Text: refused, State: "done"}}, Role: "assistant"},
		upstream.Message{Parts: []upstream.Part{{Type: "text", Text: retryMsg}}, Role: "user"},
	)
	ureq2 := ureq
	ureq2.Messages = msgs
	fmt.Println("=== RETRY upstream body (last 2 messages) ===")
	for _, m := range msgs[len(msgs)-2:] {
		for _, p := range m.Parts {
			fmt.Printf("role=%s:\n%s\n\n", m.Role, p.Text)
		}
	}
}

// lastUser mirrors the server helper: last real user text, no tool results.
func lastUser(hist []openai.History) string {
	for i := len(hist) - 1; i >= 0; i-- {
		h := hist[i]
		if h.Role != "user" || len(h.Results) > 0 {
			continue
		}
		for j := len(h.Texts) - 1; j >= 0; j-- {
			if t := h.Texts[j]; t != "" {
				return t
			}
		}
	}
	return ""
}
