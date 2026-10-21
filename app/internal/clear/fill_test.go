package clear

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Boot leaves exactly one live slot behind and starts the server; the rest of
// the ring is the background loop's work (SPEC §2.1.1, §2.1.2). A partial
// ring is a servable ring — readiness never waits for the full pool.
func TestBootFillsOnlyTheFirstSlot(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := New(Config{
		BaseURL:  cs.srv.URL,
		Pool:     5,
		Cooldown: -1,
		FillPace: -1, // no pause: the fill would otherwise still be running
		Logf:     func(string, ...any) {},
	})
	if err := c.PrewarmBoot(context.Background()); err != nil {
		t.Fatalf("PrewarmBoot: %v", err)
	}
	st := c.State()
	if st.Live != 1 || st.Pool != 5 {
		t.Fatalf("state after boot = live %d/pool %d, want 1/5: %+v", st.Live, st.Pool, st)
	}
	if !st.HasCookie {
		t.Fatal("a live slot must report has_cookie")
	}
	if got := cs.submitCount(); got != 1 {
		t.Fatalf("acquisitions at boot = %d, want exactly 1", got)
	}
	if st.Slots[0].State != SlotLive {
		t.Fatalf("slot 0 = %q, want %q", st.Slots[0].State, SlotLive)
	}
	for i := 1; i < len(st.Slots); i++ {
		if st.Slots[i].State != SlotPending {
			t.Fatalf("slot %d = %q, want %q", i, st.Slots[i].State, SlotPending)
		}
	}
}

// A failed boot prewarm is not a failed startup: PrewarmBoot reports the
// error, the service is still servable (no cookie, has_cookie false) and the
// loop keeps retrying. SPEC §2.1.2 — "a failed warm-up is not a failed
// startup".
func TestBootPrewarmFailureIsNotFatal(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	cs.failSubmit = http.StatusBadGateway
	c := New(Config{BaseURL: cs.srv.URL, Pool: 4, Cooldown: time.Millisecond, Logf: func(string, ...any) {}})
	if err := c.PrewarmBoot(context.Background()); err == nil {
		t.Fatal("PrewarmBoot must report the failure to the caller")
	}
	st := c.State()
	if st.HasCookie || st.Live != 0 {
		t.Fatalf("failed boot reported cookies: %+v", st)
	}
	if st.Pool != 4 || len(st.Slots) != 4 {
		t.Fatalf("state = %+v, want the configured ring reported truthfully", st)
	}
	if st.Slots[0].State != SlotCooldown {
		t.Fatalf("slot 0 = %q, want %q after a failed acquisition", st.Slots[0].State, SlotCooldown)
	}

	// The loop retries and the ring recovers once the edge does.
	cs.failSubmit = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := c.StartFillLoop(ctx)
	// Under -race each real PoW solve costs about a second; the property is
	// recovery, not speed.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if c.State().Live == 4 {
			cancel()
			<-done
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the fill loop never recovered the ring: %+v", c.State())
}

// The background loop fills the rest of the ring sequentially: never two
// acquisitions in flight, one per iteration, and each iteration is paced by
// the configured pause (SPEC §2.1.2).
func TestFillLoopIsSequentialAndPaced(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	const (
		pool = 4
		pace = 60 * time.Millisecond
	)
	c := New(Config{BaseURL: cs.srv.URL, Pool: pool, Cooldown: -1, FillPace: pace, Jitter: -1, Logf: func(string, ...any) {}})
	if err := c.PrewarmBoot(context.Background()); err != nil {
		t.Fatalf("PrewarmBoot: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartFillLoop(ctx)
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && c.State().Live < pool {
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.State().Live; got != pool {
		t.Fatalf("live = %d after the fill, want %d", got, pool)
	}

	tokens := cs.tokenTimes()
	if len(tokens) != pool {
		t.Fatalf("challenges = %d, want %d", len(tokens), pool)
	}
	// Sequential: each acquisition starts after the previous one finished.
	subs := cs.submissions()
	if len(subs) != pool {
		t.Fatalf("submissions = %d, want %d", len(subs), pool)
	}
	for i := 1; i < len(tokens); i++ {
		if tokens[i].Before(subs[i-1]) {
			t.Fatalf("acquisition %d started at %s before acquisition %d submitted at %s: not sequential",
				i, tokens[i], i-1, subs[i-1])
		}
	}
	// Paced: consecutive probes are at least FillPace apart.
	for i := 2; i < len(tokens); i++ { // skip boot -> first loop acquisition
		if gap := tokens[i].Sub(tokens[i-1]); gap < pace-5*time.Millisecond {
			t.Fatalf("acquisitions %d and %d are %s apart, want >= %s (jittered pace)",
				i-1, i, gap, pace)
		}
	}
}

// The same loop is what refills a slot retired by a 429. Retirement happens
// on the request path, which must not solve; the loop notices the shortfall
// and fills the empty slot on a later iteration (SPEC §2.1.2).
func TestRetiredSlotIsRefilledByTheFillLoop(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := New(Config{BaseURL: cs.srv.URL, Pool: 3, Cooldown: -1, FillPace: 10 * time.Millisecond, Jitter: -1, Logf: func(string, ...any) {}})
	if err := c.PrewarmNow(context.Background()); err != nil {
		t.Fatalf("PrewarmNow: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartFillLoop(ctx)
	defer func() { cancel(); <-done }()

	_, used := c.Header()
	before := cs.submitCount()
	start := time.Now()
	c.Retire(used)
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("Retire took %s; the request path must not solve", elapsed)
	}
	if got := cs.submitCount(); got != before {
		t.Fatalf("Retire itself submitted (%d -> %d acquisitions)", before, got)
	}
	if got := c.State().Live; got != 2 {
		t.Fatalf("live after retirement = %d, want 2", got)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && c.State().Live < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := c.State().Live; got != 3 {
		t.Fatalf("live = %d after the loop ran, want the retired slot refilled", got)
	}
}

// Refresh is smooth: each iteration replaces at most one stale slot, so a
// 20-slot ring is fully rotated inside the 45m->60m headroom without any
// burst (SPEC §2.1.2). A tick that starts with the whole ring stale replaces
// exactly one slot.
func TestFillLoopRefreshesOneSlotPerIteration(t *testing.T) {
	t.Parallel()
	const pool = 20
	cs := newChallengeServer(t)
	var mu sync.Mutex
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	c := New(Config{BaseURL: cs.srv.URL, Pool: pool, Refresh: 45 * time.Minute,
		Cooldown: -1, FillPace: 5 * time.Millisecond, Jitter: -1, Now: clock, Logf: func(string, ...any) {}})
	if err := c.PrewarmNow(context.Background()); err != nil {
		t.Fatalf("PrewarmNow: %v", err)
	}
	mu.Lock()
	now = now.Add(46 * time.Minute)
	mu.Unlock()

	before := cs.submitCount()
	if err := c.RefreshStale(context.Background()); err != nil {
		t.Fatalf("RefreshStale: %v", err)
	}
	if got := cs.submitCount() - before; got != 1 {
		t.Fatalf("one refresh iteration replaced %d slots, want exactly 1", got)
	}

	// The whole ring rotates inside the headroom: one slot per iteration at
	// the default paced cadence (FillPace + Jitter <= 2s) takes <= 40s for 20
	// slots, and even the loop's idle cadence (>= 1s) takes <= 20 minutes.
	headroom := 60*time.Minute - 45*time.Minute
	worst := time.Duration(pool) * (DefaultFillPace + DefaultJitter)
	if worst >= headroom {
		t.Fatalf("paced refresh ticks take %s for %d slots, outside the %s headroom", worst, pool, headroom)
	}
}

// A failed fill enters the per-slot cooldown and the loop retries it later;
// neither the other slots nor the service are affected (SPEC §2.1.2).
func TestFailedFillCoolsDownAndRetries(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	now := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	const cooldown = 20 * time.Second
	c := New(Config{BaseURL: cs.srv.URL, Pool: 2, Cooldown: cooldown, Now: clock, Logf: func(string, ...any) {}})

	cs.failSubmit = http.StatusBadGateway
	if err := c.acquireSlot(context.Background(), 0); err == nil {
		t.Fatal("expected slot 0's acquisition to fail")
	}
	if st := c.State().Slots[0]; st.State != SlotCooldown {
		t.Fatalf("slot 0 = %q, want %q", st.State, SlotCooldown)
	}
	// Inside the window the slot is not retried...
	before := cs.submitCount()
	if err := c.acquireSlot(context.Background(), 0); err == nil {
		t.Fatal("a retry inside the cooldown must report the failure, not solve")
	}
	if got := cs.submitCount(); got != before {
		t.Fatalf("slot retried inside its cooldown (%d -> %d)", before, got)
	}
	// ...while slot 1 fills immediately.
	cs.failSubmit = 0
	if err := c.acquireSlot(context.Background(), 1); err != nil {
		t.Fatalf("slot 1 blocked by slot 0's cooldown: %v", err)
	}
	if st := c.State(); st.Slots[1].State != SlotLive || st.Live != 1 {
		t.Fatalf("state = %+v, want slot 1 live", st)
	}
	// After the window the slot is retried and recovers.
	mu.Lock()
	now = now.Add(cooldown)
	mu.Unlock()
	if err := c.acquireSlot(context.Background(), 0); err != nil {
		t.Fatalf("slot 0 did not retry after the cooldown: %v", err)
	}
	if st := c.State().Slots[0]; st.State != SlotLive {
		t.Fatalf("slot 0 = %q after the retry, want %q", st.State, SlotLive)
	}
}

// Shutdown cancels the single fill goroutine and it exits promptly: no
// orphaned worker, no leak under -race (SPEC §2.1.2, TDD point 6).
func TestFillLoopShutdownLeavesNoWorker(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := New(Config{BaseURL: cs.srv.URL, Pool: 3, Cooldown: -1, FillPace: time.Millisecond, Jitter: -1, Logf: func(string, ...any) {}})
	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartFillLoop(ctx)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the fill loop ignored cancellation")
	}
	if got := c.State().Live; got > 3 {
		t.Fatalf("live = %d after shutdown, want at most the ring size", got)
	}
}

// The loop also acts on an in-flight context: cancelling during the paced
// pause returns immediately rather than sleeping out the remainder.
func TestFillLoopStopsPromptlyInsideThePace(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := New(Config{BaseURL: cs.srv.URL, Pool: 3, Cooldown: -1, FillPace: time.Hour, Jitter: -1, Logf: func(string, ...any) {}})
	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartFillLoop(ctx)
	// Give the loop time to finish its first acquisition and enter the pause.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && c.State().Live == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the loop slept out a one-hour pace after cancellation")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("shutdown took %s, want prompt cancellation", elapsed)
	}
	if got := cs.submitCount(); got > 1 {
		t.Fatalf("acquisitions after prompt shutdown = %d, want at most 1", got)
	}
}

// An unchallenged egress holds no cookies, so the boot fill reports success
// with nothing live and the loop does not manufacture a ring of probes.
func TestUnchallengedEgressBootsWithoutCookies(t *testing.T) {
	t.Parallel()
	var probes probeCounter
	srv := newUnchallengedServer(t, &probes)
	c := New(Config{BaseURL: srv.URL, Pool: 20, Cooldown: -1, FillPace: time.Millisecond, Jitter: -1, Logf: func(string, ...any) {}})
	if err := c.PrewarmBoot(context.Background()); err != nil {
		t.Fatalf("PrewarmBoot: %v", err)
	}
	st := c.State()
	if st.HasCookie || st.Live != 0 || st.Pool != 20 {
		t.Fatalf("state = %+v, want 0/20 live", st)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartFillLoop(ctx)
	defer func() { cancel(); <-done }()
	time.Sleep(150 * time.Millisecond)
	if got := probes.Load(); got != 1 {
		t.Fatalf("probes = %d, want the one boot probe (no manufactured ring)", got)
	}
}

type probeCounter = atomic.Int32
