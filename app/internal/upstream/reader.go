package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

// ErrStreamTruncated means the upstream closed the body before the [DONE]
// sentinel. It is never reported as success: a truncated answer must be
// distinguishable from a finished one.
var ErrStreamTruncated = errors.New("upstream: stream ended before [DONE]")

// ErrIdleTimeout means no upstream line arrived within the sliding idle
// window. It is distinct from a client disconnect and from a transport error.
var ErrIdleTimeout = errors.New("upstream: stream idle timeout")

// maxLineBytes caps one upstream line (SSE or NDJSON). The upstream sends
// whole answers in single events on this diffusion model and whole source
// lists in one docs event, so the buffer is generous.
const maxLineBytes = 16 << 20

// Reader parses an upstream SSE body and emits one callback per event.
type Reader struct {
	r           io.Reader
	idle        time.Duration
	onMalformed func(line string)
}

// NewReader wraps body. onMalformed may be nil; malformed JSON lines are
// skipped rather than aborting the stream.
func NewReader(body io.Reader, idle time.Duration, onMalformed func(string)) *Reader {
	return &Reader{r: body, idle: idle, onMalformed: onMalformed}
}

// Read consumes the stream, invoking onEvent for each data event and
// returning when [DONE] is seen, the idle watchdog fires, the context is
// cancelled, or the stream ends.
//
// The idle watchdog is sliding: every line read, including blank separators,
// resets it (lines, §6.1). A clean end before [DONE] is ErrStreamTruncated.
func (r *Reader) Read(ctx context.Context, onEvent func(Event) error) error {
	err := lines(ctx, r.r, r.idle, 0, func(line string) error {
		if !strings.HasPrefix(line, "data:") {
			return nil
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			return nil
		}
		if payload == "[DONE]" {
			return errStopLines
		}
		var ev Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil || ev.Type == "" {
			if r.onMalformed != nil {
				r.onMalformed(payload)
			}
			return nil
		}
		return onEvent(ev)
	})
	switch {
	case errors.Is(err, errStopLines):
		return nil
	case errors.Is(err, io.EOF):
		return ErrStreamTruncated
	default:
		return err
	}
}

// errStopLines is the sentinel a line callback returns to end the pump
// successfully (chat's [DONE], search's terminal event).
var errStopLines = errors.New("upstream: line pump stopped")
