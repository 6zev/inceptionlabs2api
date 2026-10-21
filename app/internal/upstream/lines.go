package upstream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// lines is the one upstream line pump: a scanner goroutine feeding a channel,
// a sliding idle timer reset by every received line, and an optional absolute
// cap independent of that timer. It exists so chat (SSE) and search (NDJSON)
// share one watchdog mechanism instead of each owning a copy (SPEC §6.1).
//
// It returns when onLine returns a stop error, the idle window expires, the
// hard cap expires, the context is cancelled, or the body ends. A body that
// ends cleanly is reported as io.EOF so each family can decide whether that is
// success (search: docs already collected) or truncation (chat: no [DONE]).
// The scanner goroutine is released through done; the caller's deferred body
// Close unblocks a pending network read.
func lines(ctx context.Context, body io.Reader, idle, hardCap time.Duration, onLine func(line string) error) error {
	if idle <= 0 {
		idle = 60 * time.Second
	}
	type lineResult struct {
		line string
		err  error
	}
	lines := make(chan lineResult)
	done := make(chan struct{})
	defer close(done)

	go func() {
		scanner := bufio.NewScanner(body)
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

	timer := time.NewTimer(idle)
	defer timer.Stop()
	var capC <-chan time.Time
	if hardCap > 0 {
		capTimer := time.NewTimer(hardCap)
		defer capTimer.Stop()
		capC = capTimer.C
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return ErrIdleTimeout
		case <-capC:
			return ErrIdleTimeout
		case res := <-lines:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
			if res.err != nil {
				if errors.Is(res.err, io.EOF) {
					return io.EOF
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("upstream: read stream: %w", res.err)
			}
			if err := onLine(res.line); err != nil {
				return err
			}
		}
	}
}
