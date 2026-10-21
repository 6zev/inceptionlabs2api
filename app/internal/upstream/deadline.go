package upstream

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The whole-call deadline is the shared mechanism for families whose upstream
// answers one complete body rather than a line stream: audio (TTS, STT) and
// decisions. It is deliberately separate from the sliding line watchdog
// (lines, §6.1) — there is no line to reset a timer with — and is documented
// next to it so nobody adds a third mechanism (SPEC §6.1).

// withCallTimeout bounds a whole call. A zero or negative timeout leaves the
// caller's context untouched.
func withCallTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// bodyReadError classifies a body-read failure. The call timeout expiring
// mid-body is the family's idle window, so it is reported as ErrIdleTimeout
// and mapped to 504 like chat; a caller cancellation stays the caller's.
func bodyReadError(ctx context.Context, what string, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return idleTimeout(what)
	}
	return fmt.Errorf("upstream: read %s body: %w", what, err)
}

// cutByDeadline reports whether this call's own timeout expired. Unlike
// bodyReadError it applies after a *successful* read: a body cut by the
// deadline can arrive as a clean EOF (the peer closes END_STREAM when the
// request is cancelled), and an empty or partial body must not then be
// reported as "the upstream sent nothing"/"unparseable".
func cutByDeadline(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.DeadlineExceeded)
}

// idleTimeout is the typed whole-call timeout error, mapped to 504.
func idleTimeout(what string) error {
	return fmt.Errorf("%w: %s body did not complete before the call deadline", ErrIdleTimeout, what)
}
