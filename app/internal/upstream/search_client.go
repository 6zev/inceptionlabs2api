package upstream

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrSearchIncomplete means the NDJSON body ended without a docs event and
// without a terminal error event. A stream is never reported as success just
// because it closed.
var ErrSearchIncomplete = errors.New("upstream: search stream ended before docs")

// SearchError is a terminal `{"t":"error"}` event. The upstream message is
// kept for server-side logging and never sent to clients verbatim.
type SearchError struct{ Message string }

// Error implements error.
func (e *SearchError) Error() string { return "upstream: search error: " + e.Message }

// Collection caps bound what a hostile or buggy upstream can make the
// gateway retain and echo. They are well above any plausible real run: the
// upstream's own maximum is fanout 5 × depth 3.
const (
	maxCollectedQueries = 32
	maxCollectedSources = 50
	maxQueryRunes       = 2000
)

// absoluteSearchCap bounds a whole run independently of the sliding idle
// window, so an upstream that emits one keep-alive line per idle period
// cannot pin a request forever. Ten idle windows is far beyond a real run
// (the smallest captured run took 3.1 s).
func absoluteSearchCap(idle time.Duration) time.Duration { return 10 * idle }

// Search runs POST /api/search-lab/run and collects the rewritten queries
// and the final docs source list. It stops reading as soon as retrieval is
// finished — the last round's docs, a synthesize call, or run_done — because
// snippets do not exist upstream and synthesis is pure latency. With depth>1
// the follow-up queries and the last round's docs are the ones that count.
//
// idle is a sliding watchdog reset by every received line; a stalled run is
// abandoned with ErrIdleTimeout. The caller owns cancellation via ctx.
func (c *Client) Search(ctx context.Context, req *SearchRequest, idle time.Duration) (*SearchResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("upstream: marshal search request: %w", err)
	}
	if idle <= 0 {
		idle = 60 * time.Second
	}
	resp, err := c.postAuthed(ctx, "/api/search-lab/run", "search", body, idle, func(r *http.Request) {
		r.Header.Set("Accept", "application/x-ndjson")
		// Go does not decompress Brotli. Asking for identity keeps the body
		// plain; a `br` response is still handled defensively below.
		r.Header.Set("Accept-Encoding", "identity")
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	reader, err := decodedBody(resp.Body, resp.Header.Get("Content-Encoding"))
	if err != nil {
		return nil, err
	}
	return collectSearch(ctx, reader, idle)
}

// decodedBody wraps body according to Content-Encoding. Identity and gzip
// (stdlib) are supported. Brotli cannot be decoded without a third-party
// dependency, so a br body is a typed error naming the encoding rather than
// mojibake; the request advertises Accept-Encoding: identity, so this only
// happens if an intermediary re-compresses.
func decodedBody(body io.Reader, encoding string) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body, nil
	case "br":
		return nil, fmt.Errorf("upstream: search body is brotli-encoded (%q) and this build has no brotli decoder", encoding)
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return nil, fmt.Errorf("upstream: search content-encoding gzip: %w", err)
		}
		return zr, nil
	default:
		return nil, fmt.Errorf("upstream: unsupported search content-encoding %q", encoding)
	}
}

// collectSearch consumes NDJSON lines into a SearchResult. Malformed lines
// and unknown event types are skipped, matching the playground's reader. It
// runs on the shared line pump (lines, §6.1) with the search hard cap, so a
// stalled body is abandoned and a keep-alive trickle cannot pin a request.
func collectSearch(ctx context.Context, r io.Reader, idle time.Duration) (*SearchResult, error) {
	res := &SearchResult{}
	seenQuery := make(map[string]bool)
	haveDocs := false
	var stopErr error

	// finish ends the pump; it is what every terminal event returns.
	finish := func() error {
		if !haveDocs {
			return ErrSearchIncomplete
		}
		return errStopLines
	}

	pumpErr := lines(ctx, r, idle, absoluteSearchCap(idle), func(line string) error {
		line = strings.TrimSpace(line)
		if line == "" {
			return nil
		}
		var ev searchEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil
		}
		switch ev.Type {
		case searchEventQueries:
			for _, q := range ev.Queries {
				if len(res.Queries) >= maxCollectedQueries {
					break
				}
				q = strings.TrimSpace(q)
				if q == "" || len([]rune(q)) > maxQueryRunes || seenQuery[q] {
					continue
				}
				seenQuery[q] = true
				res.Queries = append(res.Queries, q)
			}
		case searchEventDocs:
			res.Sources = normalizeSources(ev.Sources)
			haveDocs = true
		case searchEventRounds:
			// The last round's docs are in: retrieval is complete and
			// only follow-up questions or synthesis remain.
			if haveDocs && ev.Run >= ev.Of {
				return finish()
			}
		case searchEventCallStart:
			if ev.Kind == searchKindSynthesize {
				return finish()
			}
		case searchEventRunDone:
			return finish()
		case searchEventError:
			stopErr = &SearchError{Message: ev.Message}
			return errStopLines
		}
		return nil
	})
	if stopErr != nil {
		return nil, stopErr
	}
	switch {
	case errors.Is(pumpErr, errStopLines):
		return res, nil
	case errors.Is(pumpErr, io.EOF):
		// A docs event is already the product, so a body that closes after
		// one is a complete answer; closing before one is truncation and must
		// not look like an empty result set.
		if !haveDocs {
			return nil, ErrSearchIncomplete
		}
		return res, nil
	default:
		return nil, pumpErr
	}
}

// normalizeSources cleans upstream titles (raw HTML whitespace), keeps only
// http(s) URLs, and bounds both length and count. The upstream already
// returns rank order. A non-http URL is dropped rather than passed through:
// the documented consumer renders these as links, so a javascript:/data:
// scheme would be executable in its origin.
func normalizeSources(in []SearchSource) []SearchSource {
	out := make([]SearchSource, 0, min(len(in), maxCollectedSources))
	for _, s := range in {
		if len(out) >= maxCollectedSources {
			break
		}
		url := strings.TrimSpace(s.URL)
		if !usableSource(url) {
			continue
		}
		out = append(out, SearchSource{
			Title: sourceTitle(s.Title),
			URL:   url,
			Score: s.Score,
		})
	}
	return out
}

// SearchConfig calls GET /api/search-lab/config. The live route is challenged
// independently of /api/session and answers 401 to a cleared but unauthenticated
// request, so the probe carries both clearance and a pooled token. When the
// pool cannot supply one the probe is reported as failed (availability stays
// unknown) rather than guessed.
func (c *Client) SearchConfig(ctx context.Context) (*SearchConfigResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	// Through the one token-bearing wrapper, so a probe that meets a rejected
	// token retries like every other family instead of failing first try.
	resp, err := c.authed(ctx, http.MethodGet, "/api/search-lab/config", "search-config", nil, c.timeout, func(r *http.Request) {
		r.Header.Set("Accept", "application/json")
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("upstream: read search config: %w", err)
	}
	var cfg SearchConfigResponse
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("upstream: parse search config: %w (body %q)", err, truncate(string(raw), 200))
	}
	// A body that does not actually say retrievalAvailable must not be read as
	// "false". A checkpoint challenge is JSON without that key; treating it as
	// a kill-switch would silently disable a working endpoint.
	if !bytes.Contains(raw, []byte(`"retrievalAvailable"`)) {
		return nil, fmt.Errorf("upstream: search config response has no retrievalAvailable field (body %q)", truncate(string(raw), 200))
	}
	return &cfg, nil
}
