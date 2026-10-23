package tools

import (
	"regexp"
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
	// RecoveryRefused: the zero-call round is a canned refusal — measured
	// production shape "I'm Mercury, built by Inception. I'm not able to get
	// into the details..." (dbg4 r10) — delivered as prose with zero call
	// blocks, which ClassifyNoCall would call clean and pass through. The
	// gateway re-instructs once: the tools are real, use them.
	RecoveryRefused
)

// String names the kind for wire-log lines ([recovery kind=...]).
func (k RecoveryKind) String() string {
	switch k {
	case RecoveryTruncated:
		return "truncated"
	case RecoveryParseFailed:
		return "parse_failed"
	case RecoveryRefused:
		return "refused"
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
	// The refusal shape is checked FIRST (SDD-refusal-recovery T3): a canned
	// refusal arrives as plain prose, so the envelope logic below would call
	// it clean and pass the refusal through verbatim — the main channel
	// through which refusals reached users (dbg4 r10). The three existing
	// kinds keep their semantics unchanged below.
	if ClassifyRefusal(trimmed) {
		return RecoveryRefused
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

// ClassifyRefusal reports whether text is a canned refusal shape: prose that
// denies the model's own abilities or access instead of using the tools the
// manifest just declared. The signatures cover the measured production
// sentence (dbg4 r10: "I'm Mercury... I'm not able to get into the details")
// plus the standard English and Chinese capability-denial phrasings the same
// model family produces.
//
// False-positive cost model (the same philosophy the JSON-tail heuristic
// documents): a benign answer that happens to match costs exactly ONE
// bounded recovery retry whose result passes through verbatim — no content
// loss, no unbounded loop. Refusing to detect the real shape costs the user
// a canned refusal. Greedy beats blind here.
func ClassifyRefusal(text string) bool {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "not able to get into the details"),
		strings.Contains(lower, "i'm mercury"):
		return true
	}
	// "i cannot" / "i can't" + an access-flavored verb, and the standalone
	// denial shapes.
	if strings.Contains(lower, "i cannot") &&
		(strings.Contains(lower, "access") || strings.Contains(lower, "browse") ||
			strings.Contains(lower, "search") || strings.Contains(lower, "web")) {
		return true
	}
	if strings.Contains(lower, "unable to access") ||
		strings.Contains(lower, "i can't browse") ||
		strings.Contains(lower, "i can't access the web") {
		return true
	}
	if re := "i don't have "; strings.Contains(lower, re) {
		rest := lower[strings.Index(lower, re)+len(re):]
		if strings.HasPrefix(rest, "the ") {
			rest = rest[len("the "):]
		}
		if strings.HasPrefix(rest, "ability") || strings.HasPrefix(rest, "access") {
			return true
		}
	}
	if strings.Contains(lower, "no internet access") || strings.Contains(lower, "no web access") {
		return true
	}
	// Chinese capability-denial phrasings.
	for _, sig := range []string{"无法访问互联网", "不能联网", "没有联网", "无法联网"} {
		if strings.Contains(text, sig) {
			return true
		}
	}
	return false
}

// notePattern matches the model-switch note hermes-style clients prepend to
// the user turn: "[Note: model was just switched from X to Y via Z. Adjust
// your self-identification accordingly.]" — measured trigger (il2a-live2) of
// the canned identity refusal. One leading block, square-bracketed.
var notePattern = regexp.MustCompile(`(?s)^\s*\[Note:[^\]]*\]\s*`)

// stripSwitchNote removes a leading model-switch note from a user text so the
// refusal retry never re-injects the very identity instruction that caused
// the refusal. Non-matching text returns unchanged.
func stripSwitchNote(s string) string {
	return notePattern.ReplaceAllString(strings.TrimSpace(s), "")
}

// StripSwitchNote is the exported form for the server's retry scrubbing.
func StripSwitchNote(s string) string { return stripSwitchNote(s) }

// RetryMessageRefusal builds the retry turn for a refused round: a direct
// instruction in the same register as the truncated/parse-failed messages —
// the tools are real, re-emit the call or run it now. The refused content is
// NOT embedded (unlike the other kinds): the refusal is a canned shape the
// model must drop, not an output it must repair, and replaying it would
// only reinforce the refusal pattern the retry exists to break.
//
// userRequest is the last real user text from the history (tool-result
// turns excluded), re-anchoring the retry on the actual task. Identity
// confusion is request-structure-dependent, not intrinsic (SARVAM-2026-001:
// a system message or a tools array alone shifts a model's self-report), so
// a model handed both plus a switch note can lock onto "who am I" and emit
// the canned identity line with zero reasoning (il2a-live r3). Re-injecting
// the user's own words re-anchors attention on the task — the goal-drift
// re-anchoring pattern — which the identity reflex cannot preempt.
// A leading model-switch note is stripped first: replaying "adjust your
// self-identification" would re-fire the very reflex the retry breaks
// (il2a-live2: the note is the measured sole trigger — the identical
// request minus the note answers normally).
// Empty userRequest (no user text found) degrades to the bare instruction.
func RetryMessageRefusal(snippet, userRequest string) string {
	msg := "Your previous answer was a refusal, but the tools in this conversation are real and connected. Do not ask the user to search manually and do not describe limitations. Re-emit the COMPLETE tool call block for the request, or run the tool call now and answer from its result."
	if q := strings.TrimSpace(stripSwitchNote(userRequest)); q != "" {
		msg += "\n\nThe request the user is waiting for an answer to is:\n" + q
	}
	return msg
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
