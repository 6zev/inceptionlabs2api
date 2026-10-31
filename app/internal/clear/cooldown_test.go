package clear

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// challengelessServer answers the probe with a challenge and refuses every
// submission with the edge's expired-token status (703).
func refusingCheckpoint(t *testing.T, posted *[]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == challengePath {
			if posted != nil {
				*posted = append(*posted, r.Header.Get("x-vercel-challenge-token"))
			}
			w.WriteHeader(703)
			return
		}
		w.Header().Set("x-vercel-challenge-token", solvableToken())
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Inside a slot's failure cooldown, refresh must report the recorded failure
// without recording a new one. It used to return the previous error wrapped in
// " (retrying in …)" and acquireSlot persisted that wrap, so the recorded error
// grew by ~27 bytes per call and every later log line printed the whole chain:
// a 28 MB log after 85 minutes, ~40 GB a day, and a multi-GB heap.
func TestCooldownDoesNotGrowTheRecordedError(t *testing.T) {
	c := newTestClearance(refusingCheckpoint(t, nil))
	c.cfg.Pool = 1
	c.cfg.Cooldown = 15 * time.Second

	if err := c.Ensure(context.Background()); err == nil {
		t.Fatal("Ensure succeeded against a refusing checkpoint")
	}

	c.mu.Lock()
	recorded := c.slots[0].lastErr.Error()
	attemptedAt := c.slots[0].lastAttempt
	c.mu.Unlock()

	const calls = 200
	for i := 0; i < calls; i++ {
		err := c.Refresh(context.Background())
		if err == nil {
			t.Fatalf("call %d: Refresh succeeded inside the cooldown", i)
		}
		// One hint, not a chain of them: the recorded failure plus at most one
		// "(retrying in …)" suffix (the remaining window shrinks, so its text
		// length varies by a byte or two).
		if got := strings.Count(err.Error(), "(retrying in "); got != 1 {
			t.Fatalf("call %d: reported error carries %d retry hints: %s", i, got, err.Error())
		}
		if got := len(err.Error()); got > len(recorded)+64 {
			t.Fatalf("call %d: reported error is %d bytes for a %d-byte failure: the hint is being accumulated", i, got, len(recorded))
		}
	}

	c.mu.Lock()
	gotRecorded, gotAttempt := c.slots[0].lastErr.Error(), c.slots[0].lastAttempt
	c.mu.Unlock()

	if gotRecorded != recorded {
		t.Fatalf("recorded error grew from %d to %d bytes across %d cooldown calls", len(recorded), len(gotRecorded), calls)
	}
	if !gotAttempt.Equal(attemptedAt) {
		t.Fatalf("cooldown call moved lastAttempt from %s to %s: the window keeps extending itself", attemptedAt, gotAttempt)
	}
}

// The fill loop must not re-target a stale slot whose last refresh failed: the
// slot keeps its old cookie (so it still reads as live and stale) and the
// cooldown is the only thing asking for a pause. Re-targeting it every
// FillPace is what turned one 703 into a retry storm.
func TestFillTargetSkipsASlotInCooldown(t *testing.T) {
	c := New(Config{
		Pool:     1,
		Cooldown: 15 * time.Second,
		Refresh:  time.Minute,
		FillPace: -1,
		Jitter:   -1,
	})
	now := time.Now()

	c.mu.Lock()
	c.slots[0].cookie = "c"
	c.slots[0].acquired = now.Add(-2 * time.Minute) // stale, so the refresh branch sees it
	c.slots[0].failed = true
	c.slots[0].lastAttempt = now
	c.slots[0].lastErr = errors.New("clear: solution rejected: HTTP 703")
	c.mu.Unlock()

	if got := c.fillTargetLocked(now); got != -1 {
		t.Fatalf("fill loop targeted slot %d while its cooldown was open, want -1", got)
	}

	c.mu.Lock()
	c.slots[0].lastAttempt = now.Add(-16 * time.Second)
	c.mu.Unlock()

	if got := c.fillTargetLocked(now); got != 0 {
		t.Fatalf("fill loop did not retry slot %d after the cooldown closed, want 0", got)
	}
}

// A stored token is only re-solved while it is still inside the edge's 60 s
// TTL. A 45-minute-old slot token is answered with HTTP 703, so a refresh must
// fall back to the fresh challenge token instead of guaranteeing a failure.
func TestRefreshIgnoresAStaleIncumbentToken(t *testing.T) {
	var posted []string
	stale := "2.1.1001." + base64.StdEncoding.EncodeToString(
		[]byte("id;pre;abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789;1;bin")) + ".sig"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == challengePath {
			tok := r.Header.Get("x-vercel-challenge-token")
			posted = append(posted, tok)
			// The real edge answers 703 for a token past its TTL.
			if tok == stale {
				w.WriteHeader(703)
				return
			}
			w.Header().Add("Set-Cookie", CookieName+"=c; Path=/; Max-Age=3600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("x-vercel-challenge-token", solvableToken())
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := newTestClearance(srv)
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// The slot's token is what a 45-minute rotation would leave behind.
	c.mu.Lock()
	c.slots[0].token = stale
	c.slots[0].tokenAt = time.Now().Add(-45 * time.Minute)
	c.mu.Unlock()

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh with a stale incumbent token: %v", err)
	}

	if len(posted) == 0 {
		t.Fatal("no solution was submitted")
	}
	if got := posted[len(posted)-1]; got != solvableToken() {
		t.Fatalf("submitted token %q, want the fresh challenge token (a token past its TTL is answered 703)", got)
	}
}

// The log-safe text is bounded, so no single log line can grow without limit
// even if some future caller wraps an error's text.
func TestShortErrorBoundsALogLine(t *testing.T) {
	big := errors.New(strings.Repeat("(retrying in 13.3s) ", 10000))
	got := shortError(big)
	if len(got) > maxLoggedErrorBytes+len("...(truncated)") {
		t.Fatalf("shortError returned %d bytes, want at most %d", len(got), maxLoggedErrorBytes+len("...(truncated)"))
	}
	if !strings.HasSuffix(got, "...(truncated)") {
		t.Fatalf("shortError did not mark the cut: %q", got[len(got)-32:])
	}
}
