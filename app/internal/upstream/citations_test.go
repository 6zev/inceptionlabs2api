package upstream

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"inceptionlabs2api/internal/openai"
)

// runCapture replays the live probe fixture and returns every event.
func runCapture(t *testing.T) []Event {
	t.Helper()
	f, err := os.Open("testdata/source-url-capture.sse")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	var events []Event
	err = NewReader(f, 5*time.Second, nil).Read(context.Background(), func(ev Event) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return events
}

func TestSourceURLShapesFromCapture(t *testing.T) {
	t.Parallel()
	events := runCapture(t)

	var sawSentinel, sawSource bool
	for _, ev := range events {
		if ev.Type != EventSourceURL {
			continue
		}
		if ev.SourceID == "" && ev.URL == "" && ev.Title == "" {
			t.Fatalf("source-url fields not parsed: %+v", ev)
		}
		if ev.Title == "__searching__" {
			sawSentinel = true
			if ev.URL != "" {
				t.Fatalf("__searching__ must have an empty url, got %q", ev.URL)
			}
		} else {
			sawSource = true
			if !isHTTPURL(ev.URL) {
				t.Fatalf("real source has a non-http url: %+v", ev)
			}
		}
	}
	if !sawSentinel || !sawSource {
		t.Fatalf("capture fixture missing sentinel or source: sentinel=%v source=%v", sawSentinel, sawSource)
	}
}

func TestCollectCitationsRanksSourcesByArrival(t *testing.T) {
	t.Parallel()
	events := runCapture(t)
	var c CitationCollector
	var logLines []string
	for _, ev := range events {
		if err := c.Observe(ev, func(format string, args ...any) { logLines = append(logLines, format) }); err != nil {
			t.Fatalf("Observe: %v", err)
		}
	}
	cites := c.Citations()
	// The capture has one __searching__ sentinel and 12 real sources.
	if len(cites) != 12 {
		t.Fatalf("citations = %d, want 12", len(cites))
	}
	first := cites[0]
	if first.Type != "url_citation" || first.Index != 1 {
		t.Fatalf("first citation = %+v", first)
	}
	if first.URL != "https://autonomy.work/portfolio/uk4dwpilotresults" {
		t.Fatalf("first url = %q", first.URL)
	}
	if first.StartIndex != 0 || first.EndIndex != 0 {
		t.Fatalf("offsets must be 0/0 (unavailable), got %d/%d", first.StartIndex, first.EndIndex)
	}
	if !strings.HasPrefix(first.Title, "The results are in") {
		t.Fatalf("first title = %q", first.Title)
	}
	for i, c := range cites {
		if c.Index != i+1 {
			t.Fatalf("citation %d has index %d; ranks must be 1..N in arrival order", i, c.Index)
		}
	}
	if len(logLines) != 0 {
		t.Fatalf("unexpected log for a clean capture: %v", logLines)
	}
}

func TestCollectCitationsSentinelSemantics(t *testing.T) {
	t.Parallel()
	var c CitationCollector
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, format) }

	if err := c.Observe(Event{Type: EventSourceURL, SourceID: "__searching__", URL: "", Title: "__searching__"}, logf); err != nil {
		t.Fatalf("searching sentinel: %v", err)
	}
	if err := c.Observe(Event{Type: EventSourceURL, SourceID: "__search_error__", URL: "", Title: "__search_error__"}, logf); err != nil {
		t.Fatalf("search_error sentinel must not abort the stream: %v", err)
	}
	if got := c.Citations(); len(got) != 0 {
		t.Fatalf("sentinels produced citations: %+v", got)
	}
	if len(logs) != 1 {
		t.Fatalf("search_error should log exactly once, logs=%v", logs)
	}
	// A real source after the sentinels starts at rank 1: __searching__ does
	// not consume a rank and __search_error__ does not either.
	if err := c.Observe(Event{Type: EventSourceURL, SourceID: "u", URL: "https://a.example", Title: " A "}, logf); err != nil {
		t.Fatalf("source: %v", err)
	}
	got := c.Citations()
	if len(got) != 1 || got[0].Index != 1 || got[0].Title != "A" {
		t.Fatalf("citations = %+v, want one at rank 1 with a trimmed title", got)
	}
}

func TestCollectCitationsDropsUnusableSources(t *testing.T) {
	t.Parallel()
	var c CitationCollector
	inputs := []Event{
		{Type: EventSourceURL, SourceID: "a", URL: "", Title: "missing url"},
		{Type: EventSourceURL, SourceID: "b", URL: "javascript:alert(1)", Title: "xss"},
		{Type: EventSourceURL, SourceID: "c", URL: "data:text/html,hi", Title: "data"},
		{Type: EventSourceURL, SourceID: "d", URL: "/relative", Title: "relative"},
		{Type: EventSourceURL, SourceID: "e", URL: "https://ctl.example/\x00", Title: "control"},
		{Type: EventSourceURL, SourceID: "f", URL: "https://ok.example/" + strings.Repeat("a", 5000), Title: "long"},
		{Type: EventSourceURL, SourceID: "g", URL: "https://ok.example", Title: "Good \n\t title"},
		{Type: EventSourceURL, SourceID: "h", URL: "https://ok.example", Title: "duplicate"},
	}
	for _, ev := range inputs {
		if err := c.Observe(ev, nil); err != nil {
			t.Fatalf("Observe: %v", err)
		}
	}
	got := c.Citations()
	if len(got) != 1 {
		t.Fatalf("citations = %+v, want only the good source", got)
	}
	if got[0].URL != "https://ok.example" || got[0].Title != "Good title" {
		t.Fatalf("citation = %+v", got[0])
	}
}

func TestCollectCitationsBoundsCount(t *testing.T) {
	t.Parallel()
	var c CitationCollector
	for i := 0; i < maxCitations+10; i++ {
		url := "https://example.com/" + strings.Repeat("x", i+1)
		if err := c.Observe(Event{Type: EventSourceURL, SourceID: url, URL: url, Title: "t"}, nil); err != nil {
			t.Fatalf("Observe: %v", err)
		}
	}
	if got := len(c.Citations()); got != maxCitations {
		t.Fatalf("citations = %d, want the %d cap", got, maxCitations)
	}
}

func TestCollectCitationsIgnoresOtherEvents(t *testing.T) {
	t.Parallel()
	var c CitationCollector
	for _, ev := range []Event{
		{Type: EventTextDelta, Delta: "https://example.com is a url"},
		{Type: EventReasoningDelta, Delta: "source-url"},
	} {
		if err := c.Observe(ev, nil); err != nil {
			t.Fatalf("Observe: %v", err)
		}
	}
	if len(c.Citations()) != 0 {
		t.Fatal("non-source events must not create citations")
	}
}

func TestCollectCitationsEmptyReturnsNil(t *testing.T) {
	t.Parallel()
	var c CitationCollector
	if got := c.Citations(); got != nil {
		t.Fatalf("empty collector returned %+v, want nil so callers can omit the field", got)
	}
}

// The annotation type must round-trip through the OpenAI wire shape.
func TestCitationJSONShape(t *testing.T) {
	t.Parallel()
	cite := openai.Citation{Type: "url_citation", Index: 2, URL: "https://a.example", Title: "A", StartIndex: 0, EndIndex: 0}
	b, err := json.Marshal(cite)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"type":"url_citation","index":2,"url":"https://a.example","title":"A","start_index":0,"end_index":0}`
	if string(b) != want {
		t.Fatalf("json = %s, want %s", b, want)
	}
}
