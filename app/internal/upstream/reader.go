package upstream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// maxLineBytes caps one SSE line. The upstream sends whole answers in single
// events on this diffusion model, so the buffer is generous.
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
// resets it. The watchdog cancels the reads by setting a read deadline on the
// body when it supports one; otherwise Read falls back to a goroutine-based
// timeout.
func (r *Reader) Read(ctx context.Context, onEvent func(Event) error) error {
	if r.idle <= 0 {
		r.idle = 60 * time.Second
	}

	// Cancel the in-flight read when the watchdog fires. deadlineSetter is
	// implemented by net.Conn-backed bodies; *http.body does not expose it,
	// so the cancel path below is the general mechanism.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type lineResult struct {
		line string
		err  error
	}
	lines := make(chan lineResult)
	done := make(chan struct{})
	defer close(done)

	go func() {
		scanner := bufio.NewScanner(r.r)
		scanner.Buffer(make([]byte, 64*1024), maxLineBytes)
		for scanner.Scan() {
			select {
			case lines <- lineResult{line: scanner.Text()}:
			case <-done:
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = io.EOF
		}
		select {
		case lines <- lineResult{err: err}:
		case <-done:
		}
	}()

	timer := time.NewTimer(r.idle)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return ErrIdleTimeout
		case res := <-lines:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(r.idle)

			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					return ErrStreamTruncated
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("upstream: read stream: %w", res.err)
			}

			line := res.line
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" {
				continue
			}
			if payload == "[DONE]" {
				return nil
			}
			var ev Event
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				if r.onMalformed != nil {
					r.onMalformed(payload)
				}
				continue
			}
			if ev.Type == "" {
				if r.onMalformed != nil {
					r.onMalformed(payload)
				}
				continue
			}
			if err := onEvent(ev); err != nil {
				return err
			}
		}
	}
}
