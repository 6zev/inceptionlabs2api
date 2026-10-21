package upstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"inceptionlabs2api/internal/clear"
	"inceptionlabs2api/internal/pool"
)

// realisticSearchNDJSON is the shape captured live (RESEARCH §4.3), trimmed to
// two rounds so multi-event collection is exercised. The second round's
// follow-up query must be collected too, and the final docs list is the one
// that wins.
const realisticSearchNDJSON = `{"t":"run_start","config":{"fanout":1,"depth":1,"keepTopK":3,"effort":"instant"},"ms":0}
{"t":"call_start","id":"c62","kind":"plan","label":"Plan 1 query","round":1,"llm":true,"ms":0.629}
{"t":"call_end","id":"c62","ms":179.96,"outputTokens":15,"inputTokens":547,"answerTokens":15}
{"t":"queries","queries":["four-day workweek trial results countries"]}
{"t":"round_start","round":1,"of":2,"queries":["four-day workweek trial results countries"],"ms":180.07}
{"t":"call_start","id":"c63","kind":"retrieve","label":"Retrieve","round":1,"llm":false,"ms":180.08}
{"t":"call_end","id":"c63","ms":2175.41,"outputTokens":null,"inputTokens":0,"answerTokens":0}
{"t":"candidates","count":4,"fresh":4,"round":1}
{"t":"call_start","id":"c64","kind":"rerank","label":"Rank 4 documents","round":1,"llm":true,"ms":2175.53}
{"t":"call_end","id":"c64","ms":2402.69,"outputTokens":63,"inputTokens":883,"answerTokens":63}
{"t":"docs","retrieved":4,"kept":3,"sources":[{"title":"\n\tFour-day work week\n        \n     – Parliament of Australia\n","url":"https://www.aph.gov.au/four-day","score":0.6},{"title":"4 Day Week Research Reports — 4 Day Week Global","url":"https://www.4dayweek.com/research","score":0.6},{"title":"UK four-day week trial | Euronews","url":"https://www.euronews.com/four-day","score":0.6}]}
{"t":"rounds","run":1,"of":2,"stoppedEarly":false}
{"t":"call_start","id":"c66","kind":"followup","label":"Follow up","round":1,"llm":true,"ms":2500}
{"t":"queries","queries":["four day week productivity data","four-day workweek trial results countries"]}
{"t":"round_start","round":2,"of":2,"queries":["four day week productivity data"],"ms":2501}
{"t":"call_start","id":"c67","kind":"retrieve","label":"Retrieve","round":2,"llm":false,"ms":2502}
{"t":"call_end","id":"c67","ms":2700,"outputTokens":null,"inputTokens":0,"answerTokens":0}
{"t":"candidates","count":2,"fresh":1,"round":2}
{"t":"call_start","id":"c68","kind":"rerank","label":"Rank 2 documents","round":2,"llm":true,"ms":2701}
{"t":"call_end","id":"c68","ms":2800,"outputTokens":20,"inputTokens":400,"answerTokens":20}
{"t":"docs","retrieved":2,"kept":2,"sources":[{"title":"  OECD productivity  ","url":"https://oecd.example/productivity","score":0.9},{"title":"","url":"https://no-title.example/x","score":0.1},{"title":"dropped: no url","url":"","score":0.0}]}
{"t":"rounds","run":2,"of":2,"stoppedEarly":false}
{"t":"call_start","id":"c69","kind":"synthesize","label":"Synthesise answer","round":2,"llm":true,"ms":2801}
{"t":"answer_delta","d":"The sources describe [1][2]."}
{"t":"run_done","ms":2923.47,"answer":"The sources describe [1][2]."}
`

func TestSearchCollectsQueriesAndSources(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		io.WriteString(w, realisticSearchNDJSON)
	}
	c, _ := newTestClient(t, m, 1)
	res, err := c.Search(context.Background(), &SearchRequest{
		Query:    "four-day workweek",
		Config:   SearchConfig{Fanout: 1, Depth: 2, KeepTopK: 3, Effort: "instant"},
		Timezone: "UTC", Language: "en-US",
	}, time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	wantQueries := []string{
		"four-day workweek trial results countries",
		"four day week productivity data",
	}
	if len(res.Queries) != len(wantQueries) {
		t.Fatalf("queries = %v, want %v", res.Queries, wantQueries)
	}
	for i := range wantQueries {
		if res.Queries[i] != wantQueries[i] {
			t.Fatalf("queries = %v, want %v", res.Queries, wantQueries)
		}
	}

	if len(res.Sources) != 2 {
		t.Fatalf("sources = %+v, want the final docs list (2 usable entries)", res.Sources)
	}
	first := res.Sources[0]
	if first.URL != "https://oecd.example/productivity" {
		t.Fatalf("sources[0].URL = %q", first.URL)
	}
	if first.Title != "OECD productivity" {
		t.Fatalf("title not whitespace-collapsed: %q", first.Title)
	}
	if first.Score != 0.9 {
		t.Fatalf("sources[0].Score = %v", first.Score)
	}
	// A titleless but URL-bearing source is kept; the URL-less one is dropped.
	if res.Sources[1].URL != "https://no-title.example/x" || res.Sources[1].Title != "" {
		t.Fatalf("sources[1] = %+v", res.Sources[1])
	}

	tokens, bodies, encodings := m.searchRequests()
	if len(tokens) != 1 || !pool.ValidateToken(tokens[0]) {
		t.Fatalf("search tokens = %v", tokens)
	}
	if len(encodings) != 1 || encodings[0] != "identity" {
		t.Fatalf("Accept-Encoding = %v, want identity (Brotli is not transparent in Go)", encodings)
	}
	var body SearchRequest
	if err := json.Unmarshal([]byte(bodies[0]), &body); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if body.Query != "four-day workweek" || body.Config.Fanout != 1 ||
		body.Config.Depth != 2 || body.Config.KeepTopK != 3 ||
		body.Config.Effort != "instant" || body.Timezone != "UTC" || body.Language != "en-US" {
		t.Fatalf("request body = %+v", body)
	}
}

// The build has zero dependencies and Go has no Brotli decoder, so a br body
// (which only appears if an intermediary re-compresses) is a typed error that
// names the encoding rather than mojibake or a silent empty result.
func TestSearchBrotliBodyIsTypedError(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Encoding", "br")
		io.WriteString(w, realisticSearchNDJSON)
	}
	c, _ := newTestClient(t, m, 1)
	_, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "brotli") {
		t.Fatalf("err = %v, want a typed brotli error", err)
	}
}

func TestSearchDecodesGzipBody(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		io.WriteString(zw, `{"t":"docs","sources":[{"title":"T","url":"https://x.example"}]}`+"\n")
		zw.Close()
	}
	c, _ := newTestClient(t, m, 1)
	res, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Sources) != 1 {
		t.Fatalf("sources = %+v", res.Sources)
	}
}

func TestSearchErrorString(t *testing.T) {
	t.Parallel()
	e := &SearchError{Message: "boom"}
	if got := e.Error(); !strings.Contains(got, "boom") {
		t.Fatalf("Error() = %q", got)
	}
}

func TestSearchRejectsUnknownContentEncoding(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "zstd")
		io.WriteString(w, realisticSearchNDJSON)
	}
	c, _ := newTestClient(t, m, 1)
	_, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "content-encoding") {
		t.Fatalf("err = %v, want a content-encoding error", err)
	}
}

func TestSearchTerminalErrorEvent(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"t":"run_start","config":{},"ms":0}`+"\n")
		io.WriteString(w, `{"t":"error","message":"retrieval backend unavailable"}`+"\n")
	}
	c, _ := newTestClient(t, m, 1)
	_, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
	var se *SearchError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SearchError", err)
	}
	if !strings.Contains(se.Message, "retrieval backend unavailable") {
		t.Fatalf("message = %q", se.Message)
	}
}

func TestSearchWithoutDocsIsTruncated(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"clean EOF":   `{"t":"queries","queries":["a"]}` + "\n",
		"run_done":    `{"t":"queries","queries":["a"]}` + "\n" + `{"t":"run_done","ms":1,"answer":"x"}` + "\n",
		"empty body":  "",
		"blank lines": "\n\n\n",
	} {
		body := body
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := startUpstream(t)
			m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, body)
			}
			c, _ := newTestClient(t, m, 1)
			_, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
			if !errors.Is(err, ErrSearchIncomplete) {
				t.Fatalf("err = %v, want ErrSearchIncomplete", err)
			}
		})
	}
}

func TestSearchMalformedLinesAreSkipped(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "not json\n")
		io.WriteString(w, `{"t":"queries","queries":["a"]}`+"\n")
		io.WriteString(w, `{"t":`+"\n")
		io.WriteString(w, `{"t":"docs","sources":[{"title":"T","url":"https://x.example","score":1}]}`+"\n")
	}
	c, _ := newTestClient(t, m, 1)
	res, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Sources) != 1 || len(res.Queries) != 1 {
		t.Fatalf("res = %+v", res)
	}
}

func TestSearchEmptySourcesIsSuccess(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"t":"queries","queries":["obscure"]}`+"\n")
		io.WriteString(w, `{"t":"docs","retrieved":0,"kept":0,"sources":[]}`+"\n")
	}
	c, _ := newTestClient(t, m, 1)
	res, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Sources) != 0 {
		t.Fatalf("sources = %+v, want empty", res.Sources)
	}
}

func TestSearchIdleTimeout(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, `{"t":"queries","queries":["a"]}`+"\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	c, _ := newTestClient(t, m, 1)
	start := time.Now()
	_, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, 100*time.Millisecond)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("idle timeout took %s", elapsed)
	}
}

func TestSearchUpstreamNon200(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, "<html>Vercel Security Checkpoint</html>")
	}
	c, _ := newTestClient(t, m, 1)
	_, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want StatusError 429", err)
	}
}

func TestSearchRetriesOn401WithFreshToken(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	var attempts atomic.Int32
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"t":"docs","sources":[]}`+"\n")
	}
	c, _ := newTestClient(t, m, 1)
	if _, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
	tokens, _, _ := m.searchRequests()
	if len(tokens) != 2 || tokens[0] == tokens[1] {
		t.Fatalf("tokens = %v, want two distinct attempts", tokens)
	}
}

func TestSearchEmptyPoolIsUnavailable(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.sessionBody = `{"ok":true,"token":"short"}` // a non-clearance failure
	c, _ := newTestClient(t, m, 0)
	_, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestSearchConfig(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.configHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"modelLabel":"Mercury","retrievalAvailable":true,
		  "defaults":{"fanout":4,"depth":2,"keepTopK":5,"effort":"instant"},
		  "limits":{"minFanout":1,"maxFanout":5,"minDepth":1,"maxDepth":3}}`)
	}
	c, _ := newTestClient(t, m, 0)
	cfg, err := c.SearchConfig(context.Background())
	if err != nil {
		t.Fatalf("SearchConfig: %v", err)
	}
	if !cfg.RetrievalAvailable {
		t.Fatal("retrievalAvailable = false")
	}
	if cfg.Limits.MaxFanout != 5 || cfg.Limits.MaxDepth != 3 {
		t.Fatalf("limits = %+v", cfg.Limits)
	}
	if cfg.Defaults.Effort != "instant" || cfg.Defaults.KeepTopK != 5 {
		t.Fatalf("defaults = %+v", cfg.Defaults)
	}
}

func TestSearchConfigRejectsNon200(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.configHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	c, _ := newTestClient(t, m, 0)
	if _, err := c.SearchConfig(context.Background()); err == nil {
		t.Fatal("expected an error for a non-200 config")
	}
}

// With depth>1 the collector must keep reading past the first docs: the
// follow-up queries and the last round's docs are what the caller asked for.
func TestSearchStopsAfterLastRound(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"t":"queries","queries":["q1"]}`+"\n")
		io.WriteString(w, `{"t":"docs","sources":[{"title":"first","url":"https://first.example"}]}`+"\n")
		io.WriteString(w, `{"t":"rounds","run":1,"of":2,"stoppedEarly":false}`+"\n")
		io.WriteString(w, `{"t":"queries","queries":["q2"]}`+"\n")
		io.WriteString(w, `{"t":"docs","sources":[{"title":"second","url":"https://second.example"}]}`+"\n")
		io.WriteString(w, `{"t":"rounds","run":2,"of":2,"stoppedEarly":false}`+"\n")
		// Everything past this point would be ignored; a terminal error here
		// proves the collector stopped instead of reading on.
		io.WriteString(w, `{"t":"error","message":"should never be read"}`+"\n")
	}
	c, _ := newTestClient(t, m, 1)
	res, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Queries) != 2 || res.Queries[1] != "q2" {
		t.Fatalf("queries = %v, want both rounds", res.Queries)
	}
	if len(res.Sources) != 1 || res.Sources[0].URL != "https://second.example" {
		t.Fatalf("sources = %+v, want the last round", res.Sources)
	}
}

// depth=1 has no follow-up stage: the synthesize call ends the read.
func TestSearchStopsAtSynthesize(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"t":"queries","queries":["q1"]}`+"\n")
		io.WriteString(w, `{"t":"docs","sources":[{"title":"t","url":"https://x.example"}]}`+"\n")
		io.WriteString(w, `{"t":"rounds","run":1,"of":1,"stoppedEarly":false}`+"\n")
		io.WriteString(w, `{"t":"call_start","kind":"synthesize"}`+"\n")
	}
	c, _ := newTestClient(t, m, 1)
	res, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Sources) != 1 {
		t.Fatalf("sources = %+v", res.Sources)
	}
}

// Compile-time check that the collector tolerates the whole realistic body
// when fed as one buffer (the scanner must not split inside a JSON object).
func TestSearchSingleBuffer(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		crlf := strings.ReplaceAll(realisticSearchNDJSON, "\n", "\r\n")
		io.Copy(w, bytes.NewReader([]byte(crlf)))
	}
	c, _ := newTestClient(t, m, 1)
	res, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Sources) != 2 {
		t.Fatalf("CRLF body: sources = %+v", res.Sources)
	}
}

// A hostile upstream must not be able to make the gateway retain or echo
// unbounded state, and must not smuggle non-http URLs to a browser consumer.
func TestSearchSanitizesHostileSources(t *testing.T) {
	t.Parallel()
	sources := []string{
		`{"title":"js","url":"javascript:alert(1)"}`,
		`{"title":"data","url":"data:text/html,<script>x</script>"}`,
		`{"title":"rel","url":"/relative"}`,
		// \n is a JSON escape, so this decodes to a URL containing a real
		// control character, which must be rejected.
		`{"title":"ctl","url":"https://ok.example/\nX-Evil: 1"}`,
		`{"title":"` + strings.Repeat("long", 500) + `","url":"https://long.example"}`,
	}
	m := startUpstream(t)
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"t":"queries","queries":["`+strings.Repeat("q", maxQueryRunes+1)+`","ok"]}`+"\n")
		io.WriteString(w, `{"t":"docs","sources":[`+strings.Join(sources, ",")+`]}`+"\n")
	}
	c, _ := newTestClient(t, m, 1)
	res, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Queries) != 1 || res.Queries[0] != "ok" {
		t.Fatalf("queries = %v, want only the bounded valid query", res.Queries)
	}
	if len(res.Sources) != 1 || res.Sources[0].URL != "https://long.example" {
		t.Fatalf("sources = %+v, want only the http(s) entry", res.Sources)
	}
	if got := len([]rune(res.Sources[0].Title)); got != maxTitleRunes {
		t.Fatalf("title len = %d, want %d", got, maxTitleRunes)
	}
}

// The documented bounds are rune counts (SPEC §2.3.2) and the citation
// collector already uses runes. A multi-byte query or title must not be cut
// short just because UTF-8 spends more than one byte per rune.
func TestSearchBoundsCountRunesNotBytes(t *testing.T) {
	t.Parallel()
	// 1 500 CJK runes = 4 500 bytes: under the 2 000-rune query bound, over a
	// byte-count bound.
	longQuery := strings.Repeat("四", 1500)
	// A URL of 3 000 ASCII chars plus a host is under 4 096 runes but would
	// exceed a byte comparison only if the byte count differs; use CJK in the
	// path to make the distinction observable.
	longURL := "https://ok.example/" + strings.Repeat("路", 1000)
	longTitle := strings.Repeat("标", maxTitleRunes+10)
	m := startUpstream(t)
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"t":"queries","queries":["`+longQuery+`"]}`+"\n")
		io.WriteString(w, `{"t":"docs","sources":[{"title":"`+longTitle+`","url":"`+longURL+`"}]}`+"\n")
	}
	c, _ := newTestClient(t, m, 1)
	res, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Queries) != 1 || len([]rune(res.Queries[0])) != 1500 {
		t.Fatalf("queries = %d entries, first %d runes; want the 1 500-rune query kept",
			len(res.Queries), len([]rune(strings.Join(res.Queries, ""))))
	}
	if len(res.Sources) != 1 {
		t.Fatalf("sources = %+v, want the CJK-path URL kept", res.Sources)
	}
	if got := len([]rune(res.Sources[0].Title)); got != maxTitleRunes {
		t.Fatalf("title = %d runes, want %d", got, maxTitleRunes)
	}
}

func TestSearchCapsSourceCount(t *testing.T) {
	t.Parallel()
	parts := make([]string, 0, maxCollectedSources+20)
	for i := 0; i < maxCollectedSources+20; i++ {
		parts = append(parts, `{"title":"t","url":"https://x.example/`+strconv.Itoa(i)+`"}`)
	}
	m := startUpstream(t)
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"t":"docs","sources":[`+strings.Join(parts, ",")+`]}`+"\n")
	}
	c, _ := newTestClient(t, m, 1)
	res, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Sources) != maxCollectedSources {
		t.Fatalf("sources = %d, want capped at %d", len(res.Sources), maxCollectedSources)
	}
}

// A config body without retrievalAvailable (e.g. a checkpoint JSON error) must
// not be recorded as retrievalAvailable:false: that would silently disable a
// working endpoint.
func TestSearchConfigWithoutFieldIsUnknown(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.configHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"error":{"code":"challenge","message":"This request requires a challenge"}}`)
	}
	c, _ := newTestClient(t, m, 0)
	if _, err := c.SearchConfig(context.Background()); err == nil || !strings.Contains(err.Error(), "retrievalAvailable") {
		t.Fatalf("err = %v, want a missing-field error", err)
	}
}

// The probe must carry clearance and a session token: the live config route
// challenges an unauthenticated request independently of /api/session.
func TestSearchConfigUsesClearanceAndToken(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	var gotCookie, gotToken string
	m.configHandler = func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		gotToken = r.Header.Get("x-session-token")
		io.WriteString(w, `{"retrievalAvailable":true}`)
	}
	c, _ := newTestClient(t, m, 1)
	if _, err := c.SearchConfig(context.Background()); err != nil {
		t.Fatalf("SearchConfig: %v", err)
	}
	if gotCookie != clear.CookieName+"=cleared" {
		t.Fatalf("cookie = %q", gotCookie)
	}
	if !pool.ValidateToken(gotToken) {
		t.Fatalf("token = %q, want a pooled session token", gotToken)
	}
}
