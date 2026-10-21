package clear

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Prewarm must return immediately even when the challenge endpoint stalls:
// boot is never allowed to wait on the network.
func TestPrewarmDoesNotBlock(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // stall until the test says so
	}))
	defer srv.Close()
	defer close(release)

	c := newTestClearance(t, srv)
	start := time.Now()
	done := make(chan struct{})
	go func() {
		c.Prewarm(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Prewarm blocked on a stalled challenge endpoint")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Prewarm took %s, want an immediate return", elapsed)
	}
}

// Prewarm acquires the cookie in the background so /healthz can report it
// moments after boot.
func TestPrewarmAcquiresCookie(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := newTestClearance(t, cs.srv)
	c.Prewarm(context.Background())

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.State().HasCookie {
			if got := cs.tokenCount(); got != 1 {
				t.Fatalf("challenge fetches = %d, want 1", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("cookie never appeared after Prewarm (state %+v)", c.State())
}

// A failed acquisition is tolerated: it is logged, boot continues, and the
// next Ensure can retry.
func TestPrewarmFailureIsTolerated(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	cs.failSubmit = http.StatusBadGateway
	var logged atomic.Int32
	c := New(Config{
		BaseURL: cs.srv.URL,
		Cooldown: -1,
		Logf:    func(string, ...any) { logged.Add(1) },
	})
	c.Prewarm(context.Background())

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && logged.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if logged.Load() == 0 {
		t.Fatal("failed prewarm must be logged")
	}
	if st := c.State(); st.HasCookie {
		t.Fatalf("failed prewarm reported a cookie: %+v", st)
	}
	// A later request can still acquire once the endpoint recovers.
	cs.failSubmit = 0
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure after failed prewarm: %v", err)
	}
	if !c.State().HasCookie {
		t.Fatal("Ensure did not recover after a failed prewarm")
	}
}

// RunRefreshLoop keeps the cookie fresh from boot: with a tiny refresh age it
// re-acquires without any traffic.
func TestRefreshLoopRefreshesFromBoot(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	var mu sync.Mutex
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	c := New(Config{
		BaseURL: cs.srv.URL,
		Refresh: time.Minute,
		Now:     clock,
		Logf:    func(string, ...any) {},
	})
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("initial Ensure: %v", err)
	}
	if got := cs.tokenCount(); got != 1 {
		t.Fatalf("fetches = %d, want 1", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loopDone := make(chan struct{})
	go func() {
		c.RunRefreshLoop(ctx, 5*time.Millisecond)
		close(loopDone)
	}()

	// Age the cookie past the refresh margin; the loop must replace it
	// without any request traffic.
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cs.tokenCount() >= 2 && c.State().RefreshCount >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := cs.tokenCount(); got < 2 {
		t.Fatalf("fetches = %d, want a proactive refresh with no traffic", got)
	}
	cancel()
	select {
	case <-loopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("RunRefreshLoop did not stop on context cancellation")
	}
}

// The loop maintains an existing cookie; when none is held (an egress that is
// never challenged, or a failed prewarm) it must not spin on solves — the
// lazy Ensure path is what retries.
func TestRefreshLoopWithoutCookieDoesNotAcquire(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := newTestClearance(t, cs.srv)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	c.RunRefreshLoop(ctx, 5*time.Millisecond)
	if got := cs.tokenCount(); got != 0 {
		t.Fatalf("fetches = %d, want none without a held cookie", got)
	}
}

// The loop exits promptly on cancellation.
func TestRefreshLoopStopsOnCancel(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := newTestClearance(t, cs.srv)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.RunRefreshLoop(ctx, time.Hour)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunRefreshLoop ignored cancellation")
	}
}
