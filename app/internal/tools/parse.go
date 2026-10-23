package tools

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Wire markers. The primary format is the Hermes/Qwen <tool_call> block; the
// <tool_calls><invoke …> form is accepted as drift because the upstream model
// was observed reproducing it verbatim (TOOLCALL-PATH.md tc3).
const (
	callStart  = "<tool_call>"
	callEnd    = "</tool_call>"
	blockStart = "<tool_calls>"
	blockEnd   = "</tool_calls>"
	invokeOpen = "<invoke name="
	invokeEnd  = "</invoke>"
	paramOpen  = "<parameter name="
	paramEnd   = "</parameter>"

	// maxBlockBytes bounds one open block. Past it the buffered text is
	// emitted as prose and scanning stops, so a hostile stream cannot grow
	// the buffer without limit (DESIGN-TOOLS.md §2, §7 T17).
	maxBlockBytes = 64 << 10
)

// Byte-slice views of the start markers, hoisted once: Feed runs per chunk
// and every scan of buf goes through bytes.Index instead of a string copy
// of the whole buffer.
var (
	callStartBytes  = []byte(callStart)
	blockStartBytes = []byte(blockStart)
)

// Call is one parsed tool invocation with its arguments still JSON-encoded.
// Arguments is always a JSON object string ("{}" when the model supplied
// none), so the OpenAI wire can carry it unchanged.
type Call struct {
	Name      string
	Arguments string
}

// Parser scans accumulated model text for tool calls. It is a pure state
// machine over the bytes received so far: Feed may be called with any
// chunking, including splits inside a marker or inside a multi-byte rune.
//
// The zero value is ready to use and declares no tools; use NewParser to
// declare names. A call naming an undeclared function is prose.
type Parser struct {
	declared map[string]struct{}

	// buf holds text not yet released. Outside a block it is only the
	// potential marker prefix (and a partial rune) at the tail.
	buf    []byte
	open   string // "" outside a block; else the marker that opened it
	closer string // matching close marker

	calls []Call
}

// NewParser returns a parser that accepts only the given tool names.
func NewParser(declared []string) *Parser {
	set := make(map[string]struct{}, len(declared))
	for _, n := range declared {
		set[n] = struct{}{}
	}
	return &Parser{declared: set}
}

// Feed appends chunk and returns the content that is now safe to emit. Marker
// fragments are held back until they resolve. A completed, valid, declared
// call is appended to Calls and emits no content.
func (p *Parser) Feed(chunk []byte) []byte {
	p.buf = append(p.buf, chunk...)
	return p.drain(false)
}

// Flush ends the stream: everything still buffered is content. An open block
// is shown verbatim (with its opening marker), never turned into a call.
func (p *Parser) Flush() []byte {
	out := p.drain(true)
	if p.open != "" {
		out = append(out, p.open...)
		p.open, p.closer = "", ""
	}
	out = append(out, p.buf...)
	p.buf = nil
	return out
}

// Calls returns the calls parsed so far in appearance order.
func (p *Parser) Calls() []Call { return p.calls }

// findStart returns the earliest start marker index in buf.
func (p *Parser) findStart() (int, string) {
	ci := bytes.Index(p.buf, callStartBytes)
	bi := bytes.Index(p.buf, blockStartBytes)
	switch {
	case ci < 0 && bi < 0:
		return -1, ""
	case bi < 0 || (ci >= 0 && ci < bi):
		return ci, callStart
	default:
		return bi, blockStart
	}
}

// drain emits every resolvable byte and leaves only what must be held back.
func (p *Parser) drain(final bool) []byte {
	var out []byte
	for {
		if p.open == "" {
			idx, marker := p.findStart()
			if idx < 0 {
				cut := len(p.buf)
				if !final {
					cut -= holdback(p.buf)
					cut = runeFloor(p.buf, cut)
				}
				out = append(out, p.buf[:cut]...)
				p.buf = append(p.buf[:0], p.buf[cut:]...)
				return out
			}
			out = append(out, p.buf[:idx]...)
			p.buf = append(p.buf[:0], p.buf[idx+len(marker):]...)
			p.open, p.closer = marker, callEnd
			if marker == blockStart {
				p.closer = blockEnd
			}
			continue
		}

		if len(p.buf) > maxBlockBytes {
			out = append(out, p.open...)
			out = append(out, p.buf...)
			p.buf = p.buf[:0]
			p.open, p.closer = "", ""
			return out
		}

		end, used := p.findClose()
		if end < 0 {
			if final {
				out = append(out, p.open...)
				out = append(out, p.buf...)
				p.buf = p.buf[:0]
				p.open, p.closer = "", ""
			}
			return out
		}

		body := string(p.buf[:end])
		rest := p.buf[end+len(used):]
		opened := p.open
		p.open, p.closer = "", ""
		p.buf = append(p.buf[:0], rest...)

		if call, ok := p.parseBody(opened, body); ok {
			p.calls = append(p.calls, call)
		} else {
			out = append(out, opened...)
			out = append(out, body...)
			out = append(out, used...)
		}
	}
}

// findClose locates the closer that ends the open block. For a marker block
// (callStart) the PLURAL form is scanned first and the canonical singular
// closer second: the singular is a strict prefix of the plural (it is
// blockEnd minus the "s"), so a short-first scan would cut a genuine plural
// close one byte early and leak the stray "s" as prose — the mixed-protocol
// drift the wire showed (dbg4 r7/r8: model opens with the singular marker but
// closes with the plural form, which previously never closed the block at
// all). A foreign block keeps its own single closer.
func (p *Parser) findClose() (int, string) {
	if p.open == callStart && p.closer == callEnd {
		plural := strings.Index(string(p.buf), blockEnd)
		singular := strings.Index(string(p.buf), callEnd)
		switch {
		case plural >= 0 && (singular < 0 || plural <= singular):
			return plural, blockEnd
		case singular >= 0:
			return singular, callEnd
		default:
			return -1, ""
		}
	}
	return strings.Index(string(p.buf), p.closer), p.closer
}

// parseBody turns one closed block body into a call. It returns false for any
// shape that must be shown to the client as prose instead.
func (p *Parser) parseBody(opened, body string) (Call, bool) {
	if opened == blockStart {
		return p.parseInvokeBlock(body)
	}
	return p.parseJSONCall(body)
}

// parseJSONCall handles <tool_call>{...}</tool_call>.
func (p *Parser) parseJSONCall(body string) (Call, bool) {
	body = strings.TrimSpace(body)
	// Fences are a model habit, not part of the protocol: unwrap one pair.
	body = trimFence(body)

	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return Call{}, false
	}
	if !p.declares(raw.Name) {
		return Call{}, false
	}
	args, ok := objectJSON(raw.Arguments)
	if !ok {
		return Call{}, false
	}
	return Call{Name: raw.Name, Arguments: args}, true
}

// parseInvokeBlock handles <tool_calls><invoke name="X">…</invoke></tool_calls>.
// The OpenAI tool_calls schema has one entry per invoke, so only the first is
// returned; the caller sees the rest as prose. Normal use is one invoke.
func (p *Parser) parseInvokeBlock(body string) (Call, bool) {
	name, params, ok := parseInvoke(body)
	if !ok || !p.declares(name) {
		return Call{}, false
	}
	return Call{Name: name, Arguments: params}, true
}

// parseInvoke parses the first <invoke> in s into name and a JSON object of
// string-valued parameters.
func parseInvoke(s string) (string, string, bool) {
	i := strings.Index(s, invokeOpen)
	if i < 0 {
		return "", "", false
	}
	s = s[i+len(invokeOpen):]
	name, rest, ok := attrValue(s)
	if !ok {
		return "", "", false
	}
	j := strings.Index(rest, invokeEnd)
	if j >= 0 {
		rest = rest[:j]
	}
	// A self-closing <invoke name="x"/> carries no parameters.
	if k := strings.IndexAny(rest, ">"); k >= 0 && strings.HasPrefix(rest[:k], "/") {
		return name, "{}", true
	}

	var b strings.Builder
	b.WriteByte('{')
	first := true
	for {
		k := strings.Index(rest, paramOpen)
		if k < 0 {
			break
		}
		rest = rest[k+len(paramOpen):]
		key, after, ok := attrValue(rest)
		if !ok {
			break
		}
		rest = after
		v := strings.Index(rest, paramEnd)
		if v < 0 {
			break
		}
		val := rest[:v]
		rest = rest[v+len(paramEnd):]
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(quoteJSON(key))
		b.WriteByte(':')
		b.WriteString(quoteJSON(val))
	}
	b.WriteByte('}')
	return name, b.String(), true
}

// attrValue reads a quoted or unquoted attribute terminated by '>' and
// returns the value and the text after the '>'.
func attrValue(s string) (string, string, bool) {
	if s == "" {
		return "", "", false
	}
	if s[0] == '"' || s[0] == '\'' {
		q := s[0]
		end := strings.IndexByte(s[1:], q)
		if end < 0 {
			return "", "", false
		}
		val := s[1 : end+1]
		rest := s[end+2:]
		rest = strings.TrimPrefix(rest, ">")
		return val, rest, true
	}
	end := strings.IndexByte(s, '>')
	if end < 0 {
		return "", "", false
	}
	return strings.TrimSpace(s[:end]), s[end+1:], true
}

// objectJSON normalises an arguments value to a JSON object string.
func objectJSON(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "{}", true
	}
	if !strings.HasPrefix(trimmed, "{") {
		return "", false
	}
	var v map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return "", false
	}
	return trimmed, true
}

// trimFence unwraps a single Markdown code fence around body.
func trimFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	} else {
		return s
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}

func (p *Parser) declares(name string) bool {
	if name == "" {
		return false
	}
	_, ok := p.declared[name]
	return ok
}

// holdback returns the number of trailing bytes that are a prefix of either
// start marker (never the whole marker: a complete marker is found above).
func holdback(buf []byte) int {
	best := 0
	for _, m := range []string{callStart, blockStart} {
		max := len(m) - 1
		if len(buf) < max {
			max = len(buf)
		}
		for k := max; k > best; k-- {
			if string(buf[len(buf)-k:]) == m[:k] {
				best = k
				break
			}
		}
	}
	return best
}

// runeFloor moves cut back to the start of a UTF-8 sequence when it would
// otherwise split one. buf[:cut] is therefore always valid UTF-8.
func runeFloor(buf []byte, cut int) int {
	for cut > 0 && cut < len(buf) && !utf8.RuneStart(buf[cut]) {
		cut--
	}
	return cut
}
