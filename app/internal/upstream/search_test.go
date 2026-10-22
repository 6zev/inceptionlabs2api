package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// runCollect drives collectSearch over a fixed NDJSON body with a generous
// idle window: the watchdog must never decide a scripted test.
func runCollect(t *testing.T, body string) (*SearchResult, error) {
	t.Helper()
	return collectSearch(context.Background(), strings.NewReader(body), time.Minute)
}

func mustCollect(t *testing.T, body string) *SearchResult {
	t.Helper()
	res, err := runCollect(t, body)
	if err != nil {
		t.Fatalf("collectSearch: unexpected error: %v", err)
	}
	return res
}

// fullRun is the measured 2026-10-22 capture shape (22 NDJSON lines reduced to
// the events the collector reacts to): plan, queries, one retrieval round,
// rerank, docs, then the synthesis phase the gateway must now read through.
func fullRun(answer string) string {
	var b strings.Builder
	b.WriteString(`{"t":"run_start","config":{"fanout":3,"depth":1}}` + "\n")
	b.WriteString(`{"t":"call_start","id":"c43","kind":"plan","llm":true}` + "\n")
	b.WriteString(`{"t":"call_end","id":"c43","outputTokens":35,"inputTokens":537}` + "\n")
	b.WriteString(`{"t":"queries","queries":["go 1.27 release date","go 1.27 new features"]}` + "\n")
	b.WriteString(`{"t":"round_start","run":1,"of":1}` + "\n")
	b.WriteString(`{"t":"call_start","id":"c50","kind":"retrieve"}` + "\n")
	b.WriteString(`{"t":"call_end","id":"c50"}` + "\n")
	b.WriteString(`{"t":"candidates","count":7,"fresh":7}` + "\n")
	b.WriteString(`{"t":"call_start","id":"c52","kind":"rerank","llm":true}` + "\n")
	b.WriteString(`{"t":"call_end","id":"c52","inputTokens":1281}` + "\n")
	b.WriteString(`{"t":"docs","retrieved":7,"kept":2,"sources":[` +
		`{"title":"Go 1.27 Release Notes","url":"https://go.dev/doc/go1.27","score":0.97},` +
		`{"title":"The Go Blog: Go 1.27","url":"https://go.dev/blog/go1.27","score":0.91}]}` + "\n")
	b.WriteString(`{"t":"rounds","run":1,"of":1,"stoppedEarly":false}` + "\n")
	b.WriteString(`{"t":"call_start","id":"c54","kind":"synthesize","llm":true}` + "\n")
	b.WriteString(`{"t":"answer_first_token","ms":2302}` + "\n")
	if answer == "" {
		b.WriteString(`{"t":"answer_delta","d":"Go 1.27 was released on August 19, 2026 "}` + "\n")
		b.WriteString(`{"t":"answer_delta","d":"with the new trace-based testing [1][2]."}` + "\n")
	} else {
		b.WriteString(`{"t":"answer_delta","d":"partial"}` + "\n")
		b.WriteString(`{"t":"run_done","ms":2282,"answer":` + quote(answer) + "}" + "\n")
	}
	b.WriteString(`{"t":"call_end","id":"c54","outputTokens":115,"inputTokens":1167}` + "\n")
	return b.String()
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestCollectSearchCapturesRunDoneAnswer(t *testing.T) {
	res := mustCollect(t, fullRun("Go 1.27 was released on August 19, 2026 [1][2][3]."))
	if res.Answer != "Go 1.27 was released on August 19, 2026 [1][2][3]." {
		t.Fatalf("Answer = %q, want the run_done answer verbatim", res.Answer)
	}
	if len(res.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(res.Sources))
	}
	if len(res.Queries) != 2 {
		t.Fatalf("queries = %v, want the two rewritten queries", res.Queries)
	}
}

func TestCollectSearchFallsBackToDeltasWhenRunDoneAnswerEmpty(t *testing.T) {
	body := fullRun("")
	// Stream ends after docs + deltas, with a run_done that carries no answer.
	body += `{"t":"run_done","ms":2282,"answer":""}` + "\n"
	res := mustCollect(t, body)
	want := "Go 1.27 was released on August 19, 2026 with the new trace-based testing [1][2]."
	if res.Answer != want {
		t.Fatalf("Answer = %q, want accumulated deltas %q", res.Answer, want)
	}
}

func TestCollectSearchStreamEndAfterDocsIsComplete(t *testing.T) {
	// The body closes after docs and some deltas but never reaches run_done.
	// Retrieval succeeded, so this is a complete result with the partial
	// answer — not an error.
	res, err := runCollect(t, fullRun(""))
	if err != nil {
		t.Fatalf("stream end after docs must not be an error: %v", err)
	}
	if len(res.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(res.Sources))
	}
	if !strings.Contains(res.Answer, "August 19, 2026") {
		t.Fatalf("Answer = %q, want the deltas collected so far", res.Answer)
	}
}

func TestCollectSearchStreamEndBeforeDocsIsIncomplete(t *testing.T) {
	body := `{"t":"run_start","config":{}}` + "\n" +
		`{"t":"queries","queries":["go"]}` + "\n"
	_, err := runCollect(t, body)
	if !errors.Is(err, ErrSearchIncomplete) {
		t.Fatalf("err = %v, want ErrSearchIncomplete", err)
	}
}

func TestCollectSearchErrorBeforeDocsIsSearchError(t *testing.T) {
	body := `{"t":"run_start","config":{}}` + "\n" +
		`{"t":"error","message":"retrieval unavailable"}` + "\n"
	_, err := runCollect(t, body)
	var se *SearchError
	if !errors.As(err, &se) || se.Message != "retrieval unavailable" {
		t.Fatalf("err = %v, want SearchError", err)
	}
}

func TestCollectSearchErrorAfterDocsKeepsRetrieval(t *testing.T) {
	// Policy (documented in collectSearch): an error event after docs means
	// synthesis failed, but retrieval already succeeded. The model keeps
	// working from the sources, so the run returns the results without an
	// answer rather than failing the whole search.
	body := fullRun("")
	// fullRun("") stops after the deltas; fail the synthesis phase there.
	body += `{"t":"error","message":"synthesis failed"}` + "\n"
	res, err := runCollect(t, body)
	if err != nil {
		t.Fatalf("error after docs must keep the retrieval result: %v", err)
	}
	if len(res.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(res.Sources))
	}
	if res.Answer != "" {
		t.Fatalf("Answer = %q, want empty (synthesis failed)", res.Answer)
	}
}

func TestCollectSearchDeltasConcatenateExactly(t *testing.T) {
	// Deltas are plain JSON strings, so a rune can never split inside one
	// event; what must hold is byte-exact concatenation across events,
	// including multi-byte content and citation markers.
	const d1 = "Go 1.27 于 2026 年 8 月 19 日发布，"
	const d2 = "主要新特性是 trace-based testing [1][2][3]。"
	body := fullRun("")
	// Replace the scripted deltas with our pair.
	body = strings.Replace(body,
		`{"t":"answer_delta","d":"Go 1.27 was released on August 19, 2026 "}`,
		`{"t":"answer_delta","d":`+quote(d1)+"}", 1)
	body = strings.Replace(body,
		`{"t":"answer_delta","d":"with the new trace-based testing [1][2]."}`,
		`{"t":"answer_delta","d":`+quote(d2)+"}", 1)
	res := mustCollect(t, body)
	if res.Answer != d1+d2 {
		t.Fatalf("Answer = %q, want exact concatenation %q", res.Answer, d1+d2)
	}
}

func TestCollectSearchSynthesizeCallIsNotATerminator(t *testing.T) {
	// The old collector stopped at the synthesize call_start; the captured
	// stream proves the answer follows it, so it must not end the run.
	res := mustCollect(t, fullRun("synthesized"))
	if res.Answer != "synthesized" {
		t.Fatalf("Answer = %q, want the post-synthesis run_done answer", res.Answer)
	}
}
