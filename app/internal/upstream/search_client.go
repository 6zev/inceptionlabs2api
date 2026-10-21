package upstream

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
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

// maxSearchLineBytes caps one NDJSON line. A docs event carries the whole
// ranked list, so the buffer is generous.
const maxSearchLineBytes = 16 << 20

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

// decodedBody wraps body according to Content-Encoding. Identity and br are
// supported; anything else is a typed error rather than mojibake.
func decodedBody(body io.Reader, encoding string) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body, nil
	case "br":
		return brotli.NewReader(body), nil
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
// and unknown event types are skipped, matching the playground's reader.
// The scan runs in a helper goroutine so a stalled body can be abandoned; the
// reader is released when the caller closes the response body.
func collectSearch(ctx context.Context, r io.Reader, idle time.Duration) (*SearchResult, error) {
	type lineResult struct {
		line string
		err  error
	}
	lines := make(chan lineResult)
	done := make(chan struct{})
	defer close(done)

	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), maxSearchLineBytes)
		for sc.Scan() {
			select {
			case lines <- lineResult{line: sc.Text()}:
			case <-done:
				return
			}
		}
		err := sc.Err()
		if err == nil {
			err = io.EOF
		}
		select {
		case lines <- lineResult{err: err}:
		case <-done:
		}
	}()

	timer := time.NewTimer(idle)
	defer timer.Stop()

	res := &SearchResult{}
	seenQuery := make(map[string]bool)
	haveDocs := false
	finish := func() (*SearchResult, error) {
		if !haveDocs {
			return nil, ErrSearchIncomplete
		}
		return res, nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, ErrIdleTimeout
		case resLine := <-lines:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)

			if resLine.err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				// A docs event is already the product, so a body that closes
				// after one is a complete answer; closing before one is
				// truncation and must not look like an empty result set.
				return finish()
			}
			line := strings.TrimSpace(resLine.line)
			if line == "" {
				continue
			}
			var ev searchEvent
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				continue
			}
			switch ev.Type {
			case searchEventQueries:
				for _, q := range ev.Queries {
					if q = strings.TrimSpace(q); q != "" && !seenQuery[q] {
						seenQuery[q] = true
						res.Queries = append(res.Queries, q)
					}
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
				return nil, &SearchError{Message: ev.Message}
			}
		}
	}
}

// normalizeSources cleans upstream titles (raw HTML whitespace) and drops
// entries without a URL. The upstream already returns rank order.
func normalizeSources(in []SearchSource) []SearchSource {
	out := make([]SearchSource, 0, len(in))
	for _, s := range in {
		url := strings.TrimSpace(s.URL)
		if url == "" {
			continue
		}
		out = append(out, SearchSource{
			Title: strings.Join(strings.Fields(s.Title), " "),
			URL:   url,
			Score: s.Score,
		})
	}
	return out
}

// SearchConfig calls GET /api/search-lab/config. It needs no session token
// (the live probe succeeded without one) and is used once at startup.
func (c *Client) SearchConfig(ctx context.Context) (*SearchConfigResponse, error) {
	resp, err := c.get(ctx, "/api/search-lab/config", "search config")
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
	return &cfg, nil
}
