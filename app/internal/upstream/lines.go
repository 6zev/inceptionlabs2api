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
//
// Memory: the scanner's buffer grows to the longest line seen and is released
// by compaction when this call returns (see the seed comment below); the
// reader's read buffer is fixed by streamReadBuffer, not per line.
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
		// Two things used to make this hold far more memory than it reads.
		//
		// 1. bufio.Scanner GROWS its buffer to fit the longest line and hands
		//    that grown array back to the pool only when the read ends. The
		//    upstream sends whole answers and whole source lists as single
		//    events, so one large event pinned a MB-scale array for the rest of
		//    the response. The seed is kept small (compactLineBytes) so that a
		//    response of ordinary short lines pays 64 KiB, and the hard cap
		//    stays maxLineBytes.
		// 2. Scanner asks the reader for 4096 bytes per Read, but over TLS a
		//    single read holds ~3 records of 16 KiB (crypto/tls BufferSize).
		//    That is ~48 KiB carrying only 4 KiB of payload, so every read
		//    overran a 64 KiB seed and grew the buffer for NO reason. A larger
		//    read makes the payload pay for the records.
		//
		// If you raise the seed, raise it into the tens of KiB at most: it is
		// per concurrent response.
		scanner.Buffer(make([]byte, compactLineBytes), maxLineBytes)
		if tb, ok := body.(interface{ SetReadBuffer(int) }); ok {
			tb.SetReadBuffer(streamReadBuffer)
		}
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
