package upstream

import (
	"strings"

	"inceptionlabs2api/internal/openai"
)

// Search-phase sentinel titles. They arrive as `source-url` events and are
// not citations: __searching__ opens the search phase and __search_error__
// reports that search failed. Neither carries a URL.
const (
	SentinelSearching   = "__searching__"
	SentinelSearchError = "__search_error__"
)

// maxCitations bounds how many annotations one response may carry. The live
// capture delivered 13; the cap keeps a hostile upstream from growing an
// unbounded finish frame.
const maxCitations = 50

// CitationCollector turns upstream source-url events into OpenAI url_citation
// annotations. The upstream publishes no per-source index (SPEC §2.4), so the
// index is the source's arrival rank, 1-based, among accepted sources.
//
// The zero value is ready to use. A collector is not safe for concurrent use;
// one is owned by one response.
type CitationCollector struct {
	cites []openai.Citation
	seen  map[string]struct{}
}

// Observe inspects one event. Sentinels and unusable sources are dropped;
// __search_error__ is reported through logf (which may be nil) and never
// aborts the stream. The error return is reserved for callers that stop a
// stream from the callback; it is always nil today.
func (c *CitationCollector) Observe(ev Event, logf func(format string, args ...any)) error {
	if ev.Type != EventSourceURL {
		return nil
	}
	if ev.Title == SentinelSearching || ev.SourceID == SentinelSearching {
		return nil
	}
	if ev.Title == SentinelSearchError || ev.SourceID == SentinelSearchError {
		if logf != nil {
			logf("upstream: web search failed mid-answer (__search_error__); the answer continues without its sources")
		}
		return nil
	}

	url := strings.TrimSpace(ev.URL)
	if !isHTTPURL(url) || len([]rune(url)) > maxURLRunes || hasControl(url) {
		return nil
	}
	if c.seen == nil {
		c.seen = make(map[string]struct{})
	}
	if _, dup := c.seen[url]; dup {
		return nil
	}
	if len(c.cites) >= maxCitations {
		return nil
	}
	c.seen[url] = struct{}{}

	title := strings.Join(strings.Fields(ev.Title), " ")
	c.cites = append(c.cites, openai.Citation{
		Type:       "url_citation",
		Index:      len(c.cites) + 1,
		URL:        url,
		Title:      truncateRunes(title, maxTitleRunes),
		StartIndex: 0,
		EndIndex:   0,
	})
	return nil
}

// Citations returns the collected annotations in arrival order, or nil when
// there are none so callers can omit the field entirely.
func (c *CitationCollector) Citations() []openai.Citation {
	if len(c.cites) == 0 {
		return nil
	}
	return c.cites
}
