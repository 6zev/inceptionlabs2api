package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxDecideBytes caps one decision response body. The upstream returns a
// small typed answer map (8 questions × a few options), so this is far above
// any real body and exists only to stop an upstream that never stops writing.
const maxDecideBytes = 4 << 20

// Decide posts a decision request to /api/decide/run and returns the
// upstream body verbatim. The request bytes are the client's own web-shape
// body (SPEC §2.6); nothing is decoded, renamed, or re-validated here — the
// route's validator is the authority on what a decision may contain.
//
// The route is one whole JSON body, so timeout bounds the whole call, not
// only the header phase (the same mechanism the audio family uses): a 200
// followed by a stalled body must not hold the handler open.
func (c *Client) Decide(ctx context.Context, body []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := withCallTimeout(ctx, timeout)
	defer cancel()

	resp, err := c.postAuthed(ctx, "/api/decide/run", "decide", body, timeout, func(r *http.Request) {
		r.Header.Set("Accept", "application/json")
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDecideBytes+1))
	if err != nil {
		return nil, bodyReadError(ctx, "decision", err)
	}
	if cutByDeadline(ctx) {
		// The deadline cancelled the request and the transport handed back a
		// clean EOF with whatever bytes had arrived. Say it was a timeout
		// before anyone tries to parse a partial body.
		return nil, idleTimeout("decision")
	}
	if len(raw) > maxDecideBytes {
		return nil, fmt.Errorf("upstream: decision body exceeds %d bytes", maxDecideBytes)
	}
	if len(raw) == 0 {
		return nil, errors.New("upstream: decision response had an empty body")
	}
	if !json.Valid(raw) {
		// A challenge page or a proxy error is not a decision. The status was
		// 200, so this is an upstream shape fault, not a client error.
		return nil, fmt.Errorf("upstream: decision response is not JSON (body %q)", truncate(string(raw), 200))
	}
	return raw, nil
}
