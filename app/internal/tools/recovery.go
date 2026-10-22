package tools

import (
	"strings"
	"unicode/utf8"
)

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

// String names the kind for wire-log lines ([recovery kind=...]).
func (k RecoveryKind) String() string {
	switch k {
	case RecoveryTruncated:
		return "truncated"
	case RecoveryParseFailed:
		return "parse_failed"
	default:
		return "clean"
	}
}

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

// foreignEnvelopes lists the XML-ish envelope shapes the upstream model was
// observed reproducing (ToolForge's detector set), each with its closer. A
// detector that does not recognize a marker simply leaves it uncounted.
var foreignEnvelopes = []struct{ open, close string }{
	{"<tool_calls>", "</tool_calls>"},
	{"<tool_use>", "</tool_use>"},
}

// foreignJSONOpeners are the JSON-field openers with no closer shape; a
// single occurrence is an opened envelope.
var foreignJSONOpeners = []string{`"tool_calls":`, "function.name:"}

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
	// Foreign markers count only when the detector recognizes them. Their
	// closers are unknown to us, so any foreign open with zero parsed calls
	// is at best parse-failed and at worst truncated — truncated when it
	// never closed, which strings.Count of the closer decides.
	foreignSeen := false
	if hasOpenMarker != nil {
		for _, env := range foreignEnvelopes {
			if hasOpenMarker(env.open) {
				n := strings.Count(text, env.open)
				nOpen += n
				foreignSeen = foreignSeen || n > 0
			}
			if hasOpenMarker(env.close) {
				nClose += strings.Count(text, env.close)
			}
		}
		for _, probe := range foreignJSONOpeners {
			if hasOpenMarker(probe) {
				n := strings.Count(text, probe)
				nOpen += n
				foreignSeen = foreignSeen || n > 0
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

	if opened || foreignSeen {
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

// lastNonSpace returns the final non-space rune of s, 0 for empty. It walks
// backwards rune-by-rune (utf8.DecodeLastRuneInString) so no rune slice is
// materialized.
func lastNonSpace(s string) rune {
	for s != "" {
		r, size := utf8.DecodeLastRuneInString(s)
		if r == utf8.RuneError && size <= 1 {
			return r // malformed tail byte: treat as a real character
		}
		switch r {
		case ' ', '\t', '\n', '\r':
			s = s[:len(s)-size]
		default:
			return r
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
