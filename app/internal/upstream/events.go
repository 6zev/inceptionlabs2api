package upstream

// Event is one parsed `data:` payload from the upstream stream
// (x-vercel-ai-ui-message-stream: v1).
type Event struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	// Delta is the incremental text for reasoning-delta and text-delta.
	Delta string `json:"delta"`
	// Snapshot is the full text carried by some text-start events, which
	// Mercury sends as whole blocks rather than increments.
	Snapshot string `json:"text"`
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
