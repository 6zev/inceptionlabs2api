package upstream

// Event is one parsed `data:` payload from the upstream stream
// (x-vercel-ai-ui-message-stream: v1).
//
// Empirical note (measured 2026-10-22, 58+ rounds of wire logs incl. real
// client traffic): the upstream /api/chat endpoint emits reasoning-delta
// events SELECTIVELY. Easy prompts get a bare reasoning-start/end pair with
// nothing between (the pair gates UI spinners, so keep consuming both).
// Reasoning-heavy prompts at reasoningEffort=high DO stream real
// reasoning-delta text (observed live: multi-step analysis summaries at
// ~3s granularity). Clients see no thinking on easy turns because the
// upstream emits none — that is upstream behavior, not a gateway bug.
// DO NOT "fix" empty rounds by inventing reasoning content: only the delta
// events carry text, and the reasoning handlers in internal/server forward
// whatever arrives — empty strings included.
type Event struct {
	Type string `json:"type"`
	// Delta is the incremental text for reasoning-delta and text-delta.
	Delta string `json:"delta"`
	// Snapshot is the full text carried by some text-start events, which
	// Mercury sends as whole blocks rather than increments.
	Snapshot string `json:"text"`
	// SourceID, URL, and Title are carried by source-url events only. The
	// upstream publishes no per-source index (SPEC §2.4), and for a real
	// source sourceId repeats the URL.
	SourceID string `json:"sourceId"`
	URL      string `json:"url"`
	Title    string `json:"title"`
}

// Event types the gateway understands. Anything else is ignored.
const (
	EventReasoningStart = "reasoning-start"
	EventReasoningDelta = "reasoning-delta"
	EventReasoningEnd   = "reasoning-end"
	EventTextStart      = "text-start"
	EventTextDelta      = "text-delta"
	EventTextEnd        = "text-end"
	EventSourceURL      = "source-url"
)

// ContentText returns the content this event contributes, if any. A
// text-start may carry a full-snapshot text block, which must not be lost; a
// text-delta carries an increment.
func (e Event) ContentText() string {
	switch e.Type {
	case EventTextStart:
		return e.Snapshot
	case EventTextDelta:
		return e.Delta
	default:
		return ""
	}
}

// Reasoning returns the reasoning text this event contributes, if any.
func (e Event) Reasoning() string {
	if e.Type == EventReasoningDelta {
		return e.Delta
	}
	return ""
}
