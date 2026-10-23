package tools

import (
	"encoding/json"
	"testing"
)

// The wire markers are assembled from split literals: the raw sequences are
// meaningful to the surrounding tooling and must not appear contiguously in
// this file.
func openMarker() string       { return "<tool" + "_call>" }
func canonicalClose() string   { return "</tool" + "_call>" }
func pluralDriftClose() string { return "</tool_" + "calls>" }

// T4 (SDD-refusal-recovery §1): mixed-protocol closing. The upstream model
// sometimes opens the block with the singular marker but closes it with the
// PLURAL form (dbg4 r7/r8) — the canonical singular closer is a strict PREFIX
// of the plural one, so the scanner must try the plural (longer) form first
// or it would cut a genuine plural close one byte early and leak the "s" as
// prose.

func TestParsePluralCloserClosesMarkerBlock(t *testing.T) {
	p := NewParser([]string{"web_search"})
	body := `{"name":"web_search","arguments":{"query":"GitHub artex"}}`
	out := p.Feed([]byte(openMarker() + body + pluralDriftClose()))
	calls := p.Calls()
	if len(calls) != 1 || calls[0].Name != "web_search" {
		t.Fatalf("calls = %+v, want 1 web_search (plural closer must close the block)", calls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("arguments not a JSON object: %v", err)
	}
	if args["query"] != "GitHub artex" {
		t.Fatalf("arguments lost: %s", calls[0].Arguments)
	}
	// The fully-parsed call emits no prose at all.
	if len(out) != 0 {
		t.Fatalf("a parsed call must emit no prose, got %q", string(out))
	}
}

func TestParsePluralCloserDropsNoTail(t *testing.T) {
	// The plural close is matched whole: no "s" remnant may leak into the
	// client-visible prose (a short-before-long scan order would cut it).
	p := NewParser([]string{"web_search"})
	body := `{"name":"web_search","arguments":{"query":"q"}}`
	tail := "Here is what I found."
	var all []byte
	all = append(all, p.Feed([]byte(" pre "+openMarker()+body+pluralDriftClose()+tail))...)
	all = append(all, p.Flush()...)
	if calls := p.Calls(); len(calls) != 1 {
		t.Fatalf("calls = %+v, want one web_search", calls)
	}
	if got := string(all); got != " pre "+tail {
		t.Fatalf("emitted prose = %q, want preamble+tail with no closer remnant", got)
	}
}

func TestParseCanonicalCloserStillWins(t *testing.T) {
	// The canonical singular closer keeps working.
	p := NewParser([]string{"web_search"})
	body := `{"name":"web_search","arguments":{"query":"q"}}`
	out := p.Feed([]byte(openMarker() + body + canonicalClose()))
	if calls := p.Calls(); len(calls) != 1 || calls[0].Name != "web_search" {
		t.Fatalf("canonical closer regressed: %+v (out %q)", calls, string(out))
	}
	if len(out) != 0 {
		t.Fatalf("parsed call must emit no prose: %q", string(out))
	}
}

func TestParseSplitDeltasStillClose(t *testing.T) {
	// The wire shape from dbg4 r7: the closer arrives split across deltas
	// ("</tool_call" then ">"), possibly split anywhere — the parser buffers
	// across Feed calls, so the reassembled marker must close the block in
	// either form.
	p := NewParser([]string{"web_search"})
	body := `{"name":"web_search","arguments":{"query":"go 1.27"}}`
	var all []byte
	all = append(all, p.Feed([]byte(openMarker()+body+"</tool_"))...)
	all = append(all, p.Feed([]byte("calls>"))...)
	all = append(all, p.Flush()...)
	if calls := p.Calls(); len(calls) != 1 || calls[0].Name != "web_search" {
		t.Fatalf("split-delta plural close did not parse: %+v (out %q)", calls, string(all))
	}
}

func TestParseForeignPluralEnvelopeUnchanged(t *testing.T) {
	// The foreign <tool_calls> envelope keeps its own path: the plural
	// closer acceptance for marker blocks must not disturb it.
	p := NewParser([]string{"web_search"})
	body := `<tool_calls><invoke name="web_search"><parameter name="query">go 1.27</parameter></invoke></tool_calls>`
	_ = p.Feed([]byte(body))
	calls := p.Calls()
	if len(calls) != 1 || calls[0].Name != "web_search" {
		t.Fatalf("foreign envelope path changed: %+v", calls)
	}
}

func TestParseExtraIDFieldStillParses(t *testing.T) {
	// Measured drift (SDD §0 shape 3): the model emits an extra "id" field in
	// the call JSON. encoding/json ignores unknown fields, so this parses
	// today — the test pins the behavior against regression.
	p := NewParser([]string{"web_search"})
	out := p.Feed([]byte(openMarker() + `{"id":"call_xxx","name":"web_search","arguments":{"query":"GitHub artex"}}` + canonicalClose()))
	calls := p.Calls()
	if len(calls) != 1 || calls[0].Name != "web_search" {
		t.Fatalf("extra id field must not break parsing: %+v (out %q)", calls, string(out))
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("arguments: %v", err)
	}
	if args["query"] != "GitHub artex" {
		t.Fatalf("arguments lost behind the id field: %s", calls[0].Arguments)
	}
}
