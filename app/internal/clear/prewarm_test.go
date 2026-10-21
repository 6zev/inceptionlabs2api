package clear

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Boot fills the first slot synchronously and is bounded by the acquisition
// timeout, so a stalled challenge cannot hold the server forever: the boot
// fill fails, boot proceeds, and the service starts (SPEC §2.1.1, §2.1.2).
func TestPrewarmBootIsBounded(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // stall until the test says so
	}))
	defer srv.Close()
	defer close(release)

	c := New(Config{BaseURL: srv.URL, Timeout: 100 * time.Millisecond, Logf: func(string, ...any) {}})
	start := time.Now()
	if err := c.PrewarmBoot(context.Background()); err == nil {
		t.Fatal("a stalled boot acquisition must report the failure")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("PrewarmBoot took %s, want the acquisition timeout bound", elapsed)
	}
	if st := c.State(); st.HasCookie {
		t.Fatalf("a stalled edge reported a cookie: %+v", st)
	}
}

// Boot returns as soon as the first slot is live. The rest of the ring is the
// fill loop's job (SPEC §2.1.2): one loop, started once at boot, completes it.
func TestPrewarmBootThenLoopFillsTheRing(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	// The default ring size with a test-sized pause: the property is that the
	// loop reaches every slot, not that a production fill takes 30-60 s.
	c := New(Config{BaseURL: cs.srv.URL, Cooldown: -1, FillPace: 5 * time.Millisecond, Jitter: -1, Logf: func(string, ...any) {}})
	if err := c.PrewarmBoot(context.Background()); err != nil {
		t.Fatalf("PrewarmBoot: %v", err)
	}
	if got := c.State().Live; got != 1 {
		t.Fatalf("boot left %d live, want exactly 1", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := c.StartFillLoop(ctx)

	// A deadline, not a latency claim: under -race each real PoW solve costs
	// about a second, so 19 paced acquisitions are slow. The property is that
	// the loop reaches every slot.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if st := c.State(); st.Live == DefaultPool {
			cancel()
			<-done
			if got := cs.tokenCount(); got != DefaultPool {
				t.Fatalf("challenge fetches = %d, want one per slot", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ring never filled after boot (state %+v)", c.State())
}

// A failed boot acquisition is reported but tolerated: boot continues, and
// the loop recovers the ring once the endpoint does (SPEC §2.1.2 — a failed
// warm-up is not a failed startup).
func TestPrewarmFailureIsTolerated(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	cs.failSubmit = http.StatusBadGateway
	c := New(Config{
		BaseURL:  cs.srv.URL,
		Pool:     3,
		Cooldown: time.Millisecond,
		FillPace: time.Millisecond,
		Jitter:   -1,
		Logf:     func(string, ...any) {},
	})
	if err := c.PrewarmBoot(context.Background()); err == nil {
		t.Fatal("a failed boot acquisition must be reported")
	}
	if st := c.State(); st.HasCookie {
		t.Fatalf("failed prewarm reported a cookie: %+v", st)
	}

	cs.failSubmit = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := c.StartFillLoop(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c.State().Live == 3 {
			cancel()
			<-done
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the loop did not recover after a failed boot: %+v", c.State())
}

// The fill loop keeps the ring fresh from boot: with a tiny refresh age it
// re-acquires without any request traffic.
func TestFillLoopRefreshesFromBoot(t *testing.T) {
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
		BaseURL:  cs.srv.URL,
		Pool:     1,
		Refresh:  time.Minute,
		FillPace: 5 * time.Millisecond,
		Jitter:   -1,
		Now:      clock,
		Logf:     func(string, ...any) {},
	})
	if err := c.PrewarmBoot(context.Background()); err != nil {
		t.Fatalf("PrewarmBoot: %v", err)
	}
	if got := cs.tokenCount(); got != 1 {
		t.Fatalf("fetches = %d, want 1", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := c.StartFillLoop(ctx)

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
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the fill loop did not stop on context cancellation")
	}
}

// On an egress that is never challenged the boot probe records the fact and
// the loop does not spin on solves: there is no cookie to hold or refresh.
func TestFillLoopWithoutCookieDoesNotAcquire(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := newTestClearance(t, cs.srv)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	c.StartFillLoop(ctx)
	if got := cs.tokenCount(); got != 0 {
		t.Fatalf("fetches = %d, want none before a boot probe", got)
	}
}

// The loop exits promptly on cancellation.
func TestFillLoopStopsOnCancel(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := newTestClearance(t, cs.srv)
	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartFillLoop(ctx)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartFillLoop ignored cancellation")
	}
}
