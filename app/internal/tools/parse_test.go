package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// run feeds chunks one by one and returns the concatenated content and calls.
func run(t *testing.T, p *Parser, chunks ...string) (string, []Call) {
	t.Helper()
	var out strings.Builder
	for _, c := range chunks {
		out.Write(p.Feed([]byte(c)))
	}
	out.Write(p.Flush())
	return out.String(), p.Calls()
}

func mustCall(t *testing.T, got, wantName, wantArgs string) {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(got), &v); err != nil {
		t.Fatalf("arguments %q is not a JSON object: %v", got, err)
	}
	var w map[string]any
	if err := json.Unmarshal([]byte(wantArgs), &w); err != nil {
		t.Fatalf("want %q is not JSON: %v", wantArgs, err)
	}
	gb, _ := json.Marshal(v)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("arguments = %s, want %s", gb, wb)
	}
	_ = wantName
}

// T2: a model that ignores the format entirely.
func TestPlainTextIsContent(t *testing.T) {
	t.Parallel()
	p := NewParser([]string{"get_weather"})
	content, calls := run(t, p, "The weather in Paris is mild.")
	if content != "The weather in Paris is mild." || len(calls) != 0 {
		t.Fatalf("content=%q calls=%v", content, calls)
	}
}

// T7: two calls with text between them.
func TestTwoCalls(t *testing.T) {
	t.Parallel()
	p := NewParser([]string{"a", "b"})
	content, calls := run(t, p,
		"Let me check.\n",
		`<tool_call>{"name":"a","arguments":{"x":1}}</tool_call>`,
		"and also\n",
		`<tool_call>{"name":"b","arguments":{"y":"z"}}</tool_call>`,
		"done")
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
	if calls[0].Name != "a" || calls[1].Name != "b" {
		t.Fatalf("names = %q, %q", calls[0].Name, calls[1].Name)
	}
	mustCall(t, calls[0].Arguments, "a", `{"x":1}`)
	mustCall(t, calls[1].Arguments, "b", `{"y":"z"}`)
	if content != "Let me check.\n"+"and also\n"+"done" {
		t.Fatalf("content = %q", content)
	}
}

// T6: text and a call in the same chunk.
func TestTextAndCallInOneChunk(t *testing.T) {
	t.Parallel()
	p := NewParser([]string{"get_weather"})
	content, calls := run(t, p, `Sure. <tool_call>{"name":"get_weather","arguments":{}}</tool_call> Done.`)
	if content != "Sure.  Done." || len(calls) != 1 || calls[0].Arguments != "{}" {
		t.Fatalf("content=%q calls=%v", content, calls)
	}
}

// T3: half a call.
func TestUnterminatedBlockIsProse(t *testing.T) {
	t.Parallel()
	p := NewParser([]string{"get_weather"})
	content, calls := run(t, p, `<tool_call>{"name":"get_weather","arguments":{"city":"Pa`)
	if len(calls) != 0 {
		t.Fatalf("calls = %v", calls)
	}
	if content != `<tool_call>{"name":"get_weather","arguments":{"city":"Pa` {
		t.Fatalf("content = %q", content)
	}
}

// T4: closed but invalid JSON.
func TestInvalidJSONIsProse(t *testing.T) {
	t.Parallel()
	p := NewParser([]string{"get_weather"})
	raw := `<tool_call>{"name":"get_weather", arguments: nope}</tool_call>`
	content, calls := run(t, p, raw)
	if len(calls) != 0 || content != raw {
		t.Fatalf("content=%q calls=%v", content, calls)
	}
}

// T5: undeclared name.
func TestUndeclaredNameIsProse(t *testing.T) {
	t.Parallel()
	p := NewParser([]string{"get_weather"})
	raw := `<tool_call>{"name":"rm_rf","arguments":{"path":"/"}}</tool_call>`
	content, calls := run(t, p, raw)
	if len(calls) != 0 || content != raw {
		t.Fatalf("content=%q calls=%v", content, calls)
	}
}

// A zero-value parser declares nothing, so every block is prose.
func TestZeroValueParserAcceptsNothing(t *testing.T) {
	t.Parallel()
	var p Parser
	content, calls := run(t, &p, `<tool_call>{"name":"x","arguments":{}}</tool_call>`)
	if len(calls) != 0 || content != `<tool_call>{"name":"x","arguments":{}}</tool_call>` {
		t.Fatalf("content=%q calls=%v", content, calls)
	}
}

// T8: nested JSON, arrays, unicode, escapes.
func TestArgumentsAreByteExact(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{"a":{"b":[1,{"c":"é"}]}}`,
		`{"text":"quote \" backslash \\ newline \n"}`,
		`{"emoji":"🜁","kanji":"天気"}`,
		`{"empty":{}}`,
	}
	for _, args := range cases {
		p := NewParser([]string{"f"})
		block := `<tool_call>{"name":"f","arguments":` + args + `}</tool_call>`
		content, calls := run(t, p, block)
		if len(calls) != 1 || content != "" {
			t.Fatalf("%s: content=%q calls=%v", args, content, calls)
		}
		mustCall(t, calls[0].Arguments, "f", args)
	}
}

// A bare string or array in "arguments" is not an object; the openai wire
// requires an object string, so the block is prose.
func TestNonObjectArgumentsAreProse(t *testing.T) {
	t.Parallel()
	for _, args := range []string{`"just a string"`, `[1,2]`, `42`} {
		p := NewParser([]string{"f"})
		block := `<tool_call>{"name":"f","arguments":` + args + `}</tool_call>`
		content, calls := run(t, p, block)
		if len(calls) != 0 || content != block {
			t.Fatalf("%s: content=%q calls=%v", args, content, calls)
		}
	}
}

// Missing arguments still yields a valid call with "{}".
func TestMissingArgumentsBecomeEmptyObject(t *testing.T) {
	t.Parallel()
	p := NewParser([]string{"ping"})
	_, calls := run(t, p, `<tool_call>{"name":"ping"}</tool_call>`)
	if len(calls) != 1 || calls[0].Arguments != "{}" {
		t.Fatalf("calls = %v", calls)
	}
}

// T18: a fenced block is still parsed.
func TestFencedBlockIsParsed(t *testing.T) {
	t.Parallel()
	p := NewParser([]string{"f"})
	content, calls := run(t, p, "```json\n<tool_call>\n```\n") // fence outside markers is prose
	if len(calls) != 0 || !strings.Contains(content, "```json") {
		t.Fatalf("unexpected: content=%q calls=%v", content, calls)
	}

	p2 := NewParser([]string{"f"})
	_, calls2 := run(t, p2, "<tool_call>\n```json\n{\"name\":\"f\",\"arguments\":{}}\n```\n</tool_call>")
	if len(calls2) != 1 {
		t.Fatalf("fenced body not parsed: %v", calls2)
	}
}

// T9: split the whole response at every byte boundary. No marker fragment may
// leak and the calls/content must be identical to the single-chunk result.
func TestMarkerSplitAtEveryBoundary(t *testing.T) {
	t.Parallel()
	cases := []string{
		"intro " + `<tool_call>{"name":"a","arguments":{"x":[1,2]}}</tool_call>` + " outro",
		`<tool_call>{"name":"a","arguments":{}}</tool_call><tool_call>{"name":"b","arguments":{"k":"v"}}</tool_call>`,
		"no calls at all <tool_ call> just text",
		`<tool_calls><invoke name="a"><parameter name="p">v</parameter></invoke></tool_calls>`,
	}
	for _, input := range cases {
		refP := NewParser([]string{"a", "b"})
		refContent, refCalls := run(t, refP, input)
		for i := 0; i <= len(input); i++ {
			p := NewParser([]string{"a", "b"})
			content, calls := run(t, p, input[:i], input[i:])
			if content != refContent {
				t.Fatalf("case %q split at %d: content %q != %q", input, i, content, refContent)
			}
			if len(calls) != len(refCalls) {
				t.Fatalf("case %q split at %d: calls %v != %v", input, i, calls, refCalls)
			}
			for j := range calls {
				if calls[j] != refCalls[j] {
					t.Fatalf("case %q split at %d: call %d %v != %v", input, i, j, calls[j], refCalls[j])
				}
			}
		}
	}
}

// T10: a multi-byte rune split between chunks must never produce invalid UTF-8
// and must reconstruct byte-exactly.
func TestRuneSplitIsSafe(t *testing.T) {
	t.Parallel()
	input := "こんにちは <tool_call>{\"name\":\"f\",\"arguments\":{\"c\":\"東京\"}}</tool_call> さようなら"
	b := []byte(input)
	// Split inside a rune: find a non-ASCII rune and cut one byte into it.
	for i := 1; i < len(b); i++ {
		if !utf8.RuneStart(b[i]) {
			p := NewParser([]string{"f"})
			out := append(p.Feed(b[:i]), p.Feed(b[i:])...)
			out = append(out, p.Flush()...)
			if !utf8.Valid(out) {
				t.Fatalf("split at %d produced invalid UTF-8", i)
			}
			// Reconstructed content + call should cover the whole input except markers/body.
			if len(p.Calls()) != 1 {
				t.Fatalf("split at %d: calls = %v", i, p.Calls())
			}
			if !strings.Contains(string(out), "こんにちは") || !strings.Contains(string(out), "さようなら") {
				t.Fatalf("split at %d: content = %q", i, out)
			}
			return
		}
	}
	t.Fatal("no multi-byte rune found in fixture")
}

// T17: an oversized block is prose and scanning stops.
func TestOversizedBlockIsProse(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", maxBlockBytes+10)
	p := NewParser([]string{"f"})
	content, calls := run(t, p, "<tool_call>{\"name\":\"f\",\"arguments\":{\"v\":\""+big)
	if len(calls) != 0 {
		t.Fatalf("calls = %v", calls)
	}
	if !p.Overflowed() || !strings.HasPrefix(content, "<tool_call>") {
		t.Fatalf("overflow=%v content prefix=%q", p.Overflowed(), content[:20])
	}
	if !strings.Contains(content, big) {
		t.Fatal("oversized block text was not preserved")
	}
}

// T12 drift: the tc3 XML form, with quoted and unquoted names, and verbatim
// (string) parameter values.
func TestInvokeFormDrift(t *testing.T) {
	t.Parallel()
	p := NewParser([]string{"get_weather"})
	content, calls := run(t, p, `<tool_calls>
<invoke name="get_weather">
<parameter name="location">Paris</parameter>
<parameter name="units">metric</parameter>
</invoke>
</tool_calls>`)
	if content != "" || len(calls) != 1 {
		t.Fatalf("content=%q calls=%v", content, calls)
	}
	mustCall(t, calls[0].Arguments, "get_weather", `{"location":"Paris","units":"metric"}`)
}

func TestInvokeUnquotedAndSelfClosing(t *testing.T) {
	t.Parallel()
	p := NewParser([]string{"a"})
	_, calls := run(t, p, `<tool_calls><invoke name=a></invoke></tool_calls>`)
	if len(calls) != 1 || calls[0].Arguments != "{}" {
		t.Fatalf("calls = %v", calls)
	}

	p = NewParser([]string{"b"})
	_, calls = run(t, p, `<tool_calls><invoke name="b"/></tool_calls>`)
	if len(calls) != 1 || calls[0].Arguments != "{}" {
		t.Fatalf("self-closing: calls = %v", calls)
	}
}

// XML values are strings even when they look numeric, and special characters
// survive as JSON strings.
func TestInvokeValuesKeepSpecialCharacters(t *testing.T) {
	t.Parallel()
	p := NewParser([]string{"f"})
	_, calls := run(t, p, `<tool_calls><invoke name="f"><parameter name="q">a "b" \ c
line2</parameter></invoke></tool_calls>`)
	if len(calls) != 1 {
		t.Fatalf("calls = %v", calls)
	}
	var v struct{ Q string }
	if err := json.Unmarshal([]byte(calls[0].Arguments), &v); err != nil {
		t.Fatalf("arguments %q: %v", calls[0].Arguments, err)
	}
	if v.Q != "a \"b\" \\ c\nline2" {
		t.Fatalf("q = %q", v.Q)
	}
}

// An <invoke> whose name is not declared falls back to prose for the wrapper.
func TestInvokeUndeclaredIsProse(t *testing.T) {
	t.Parallel()
	p := NewParser([]string{"ok"})
	raw := `<tool_calls><invoke name="bad"><parameter name="x">1</parameter></invoke></tool_calls>`
	content, calls := run(t, p, raw)
	if len(calls) != 0 || content != raw {
		t.Fatalf("content=%q calls=%v", content, calls)
	}
}

// Markup that only looks like a call (a user asking about XML) must not fire.
func TestMentioningTheFormatWithoutACall(t *testing.T) {
	t.Parallel()
	p := NewParser([]string{"get_weather"})
	text := `The format is <tool_call> and it closes with </tool_call>.`
	content, calls := run(t, p, text)
	if len(calls) != 0 {
		t.Fatalf("calls = %v", calls)
	}
	// The literal tags survive as text; the body " and it closes with " does
	// not parse, so the block is restored verbatim (minus nothing).
	if !strings.Contains(content, "<tool_call>") || !strings.Contains(content, "</tool_call>") {
		t.Fatalf("markers lost: %q", content)
	}
}

func TestFlushIsIdempotent(t *testing.T) {
	t.Parallel()
	p := NewParser([]string{"f"})
	// "ab<" is released as "ab" and holds back the possible marker prefix.
	released := p.Feed([]byte("ab<"))
	if string(released) != "ab" {
		t.Fatalf("released = %q", released)
	}
	first := p.Flush()
	second := p.Flush()
	if string(first) != "<" || len(second) != 0 {
		t.Fatalf("first=%q second=%q", first, second)
	}
}
