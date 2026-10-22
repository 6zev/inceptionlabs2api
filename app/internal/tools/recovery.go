package tools

import "strings"

// RecoveryKind classifies a zero-call round by whether it looks like a broken
// attempt at a tool call and therefore deserves one recovery retry.
//
// Philosophy (ported from ToolForge fc/recovery.py): the gateway translates,
// never blocks. Clean prose is a legal terminal answer and is never retried;
// only an output that opens a call envelope it never closes, or closes one
// whose body did not parse, is retry-worthy.
type RecoveryKind int

const (
	// RecoveryClean is plain prose with no call markers anywhere. NOT
	// retry-worthy: pass through verbatim.
	RecoveryClean RecoveryKind = iota
	// RecoveryTruncated: the text opens a call envelope but never closes it
	// (upstream cut, token cap, idle watchdog), or its JSON tail is an
	// unbalanced object/array. Retry-worthy.
	RecoveryTruncated
	// RecoveryParseFailed: the text contains call-envelope-ish openers that
	// did balance, but none produced a parseable call. Retry-worthy.
	RecoveryParseFailed
)

// retrySnippetLimit bounds the original-output tail embedded in a retry
// message, so a long broken round cannot balloon the retry turn.
const retrySnippetLimit = 2000

// ForeignOpenMarker reports whether s opens a foreign call-envelope shape the
// upstream model was observed reproducing (ToolForge's detector set): the
// XML-ish variants and the JSON field openers. Our own <tool_call> marker is
// recognized separately by the classifier and must NOT be routed through this
// detector, or a closed our-protocol block would be double-counted.
func ForeignOpenMarker(s string) bool {
	switch {
	case strings.Contains(s, "<tool_calls>"),
		strings.Contains(s, "<tool_use>"),
		strings.Contains(s, `"tool_calls":`),
		strings.Contains(s, "function.name:"):
		return true
	}
	return false
}

// ClassifyNoCall classifies a round that produced zero parsed calls.
// hasOpenMarker, when non-nil, additionally recognizes foreign envelope
// shapes; nil restricts classification to our own protocol.
//
// Truncation is decided two ways, matching the live failure modes:
//  1. an opened envelope without a matching close — `nOpen > nClose` over
//     the relevant markers, a token-wise count so a marker split across
//     content cannot hide;
//  2. an open JSON tail: the last non-space character is `,` `:` `{` or `[`
//     AND the tail holds unbalanced `{`/`[` — brace-guarded so ordinary
//     prose ending in a colon stays clean.
//
// Empty text is clean: the empty-answer retry is the server's separate
// existing behavior.
func ClassifyNoCall(text string, hasOpenMarker func(string) bool) RecoveryKind {
	if text == "" {
		return RecoveryClean
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return RecoveryClean
	}

	// Envelope balance over our own protocol markers.
	nOpen := strings.Count(text, callStart)
	nClose := strings.Count(text, callEnd)
	// Foreign openers count as opens only when a detector is attached; their
	// closers are unknown to us, so any foreign open with zero parsed calls
	// is at best parse-failed and at worst truncated — truncated when it
	// never closed, which strings.Count of the closer decides.
	foreignClosed := false
	if hasOpenMarker != nil {
		for _, probe := range []string{
			"<tool_calls>", "</tool_calls>",
			"<tool_use>", "</tool_use>",
		} {
			if hasOpenMarker(probe) {
				if strings.HasSuffix(probe, ">") && !strings.Contains(probe, "</") {
					nOpen += strings.Count(text, probe)
				} else {
					nClose += strings.Count(text, probe)
				}
			}
		}
		// The JSON-field openers ("tool_calls":, function.name:) have no
		// closer shape; a single occurrence is an opened envelope.
		for _, probe := range []string{`"tool_calls":`, "function.name:"} {
			if hasOpenMarker(probe) {
				nOpen += strings.Count(text, probe)
				foreignClosed = foreignClosed || strings.Count(text, probe) > 0
			}
		}
	}
	opened := nOpen > 0
	if opened && nOpen > nClose {
		return RecoveryTruncated
	}

	// Open JSON tail: last meaningful character continues a JSON value and
	// braces/brackets are unbalanced.
	last := lastNonSpace(trimmed)
	if last == ',' || last == ':' || last == '{' || last == '[' {
		if unbalancedJSONTail(trimmed) {
			return RecoveryTruncated
		}
	}

	if opened || foreignClosed {
		return RecoveryParseFailed
	}
	return RecoveryClean
}

// unbalancedJSONTail reports whether s holds more opening than closing
// braces/brackets outside JSON string literals. Prose containing a stray
// unbalanced brace is rare but possible; combined with the last-character
// guard it is precise enough for retry classification, and a false positive
// costs exactly one bounded retry whose result passes through verbatim.
func unbalancedJSONTail(s string) bool {
	depth := 0
	inString := false
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			escaped = false
		case inString:
			switch r {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case r == '"':
			inString = true
		case r == '{' || r == '[':
			depth++
		case r == '}' || r == ']':
			depth--
			if depth < 0 {
				depth = 0 // a superfluous closer earlier in prose must not
				// mask a genuinely open tail later
			}
		}
	}
	return depth > 0
}

// lastNonSpace returns the final non-space rune of s, 0 for empty.
func lastNonSpace(s string) rune {
	runes := []rune(s)
	for i := len(runes) - 1; i >= 0; i-- {
		switch runes[i] {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			return runes[i]
		}
	}
	return 0
}

// RetryMessageTruncated builds the user turn appended after the assistant's
// truncated output when the recovery retry re-asks the upstream. snippet is
// the original round's output (it is tail-clamped here).
func RetryMessageTruncated(snippet string) string {
	return "Your previous tool-call output was truncated mid-envelope. Re-emit the COMPLETE tool call protocol block only, with no extra commentary. Your previous output was:\n\n" +
		snippetTail(snippet)
}

// RetryMessageParseFailed builds the retry turn for output that opened a
// call envelope but produced no parseable call.
func RetryMessageParseFailed(snippet string) string {
	return "Your previous tool-call output could not be parsed. Re-emit a valid tool call protocol block for the listed tools only. Your previous output was:\n\n" +
		snippetTail(snippet)
}

// snippetTail clamps the embedded original output to its last
// retrySnippetLimit bytes, cut at a line boundary when possible so the model
// sees whole lines.
func snippetTail(s string) string {
	if len(s) <= retrySnippetLimit {
		return s
	}
	tail := s[len(s)-retrySnippetLimit:]
	if nl := strings.IndexByte(tail, '\n'); nl >= 0 && nl < len(tail)/2 {
		tail = tail[nl+1:]
	}
	return tail
}
