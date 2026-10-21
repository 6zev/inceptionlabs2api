package clear

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The ring is sized by config and the boot prewarm fills every slot
// (SPEC §2.1.2). Three acquisitions are sequential, not a burst.
func TestRingPrewarmFillsEverySlot(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := New(Config{BaseURL: cs.srv.URL, Pool: 3, Logf: func(string, ...any) {}})
	if err := c.PrewarmNow(context.Background()); err != nil {
		t.Fatalf("PrewarmNow: %v", err)
	}
	st := c.State()
	if len(st.Slots) != 3 {
		t.Fatalf("slots = %d, want 3", len(st.Slots))
	}
	for i, s := range st.Slots {
		if !s.Cookie {
			t.Fatalf("slot %d empty after prewarm: %+v", i, st.Slots)
		}
	}
	if got := cs.submitCount(); got != 3 {
		t.Fatalf("acquisitions = %d, want one per slot", got)
	}
	if !st.HasCookie || st.RefreshCount != 3 {
		t.Fatalf("top-level state = %+v, want a cookie and 3 acquisitions", st)
	}
}

// A pooled size of one is the single-cookie behaviour: one slot, one solve.
func TestRingSizeOneIsSingleCookie(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := New(Config{BaseURL: cs.srv.URL, Pool: 1, Logf: func(string, ...any) {}})
	if err := c.PrewarmNow(context.Background()); err != nil {
		t.Fatalf("PrewarmNow: %v", err)
	}
	st := c.State()
	if len(st.Slots) != 1 || !st.Slots[0].Cookie {
		t.Fatalf("state = %+v, want exactly one filled slot", st)
	}
	if got := cs.submitCount(); got != 1 {
		t.Fatalf("acquisitions = %d, want 1", got)
	}
	for i := 0; i < 5; i++ {
		if _, cookie := c.Header(); cookie == "" {
			t.Fatalf("request %d got no cookie", i)
		}
	}
}

// Header() rotates once per request: n requests over k slots give every slot
// n/k turns (SPEC §2.1.2 — rotation is per request, not per token).
func TestRingRotatesPerRequest(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := New(Config{BaseURL: cs.srv.URL, Pool: 3, Logf: func(string, ...any) {}})
	if err := c.PrewarmNow(context.Background()); err != nil {
		t.Fatalf("PrewarmNow: %v", err)
	}
	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		_, cookie := c.Header()
		if cookie == "" {
			t.Fatalf("request %d got no cookie", i)
		}
		counts[cookie]++
	}
	if len(counts) != 3 {
		t.Fatalf("requests saw %d distinct cookies, want all 3 slots: %v", len(counts), counts)
	}
	for cookie, n := range counts {
		if n != 3 {
			t.Fatalf("cookie %q used %d times, want 3", cookie, n)
		}
	}
}

// Ensure acquires when the ring is empty and is a no-op once any slot is live:
// topping the ring up is boot's and the refiller's job, never the hot path.
func TestRingEnsureOnlyAcquiresWhenEmpty(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := New(Config{BaseURL: cs.srv.URL, Pool: 3, Cooldown: -1, Logf: func(string, ...any) {}})
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := cs.submitCount(); got != 1 {
		t.Fatalf("acquisitions = %d, want 1 (Ensure serves, it does not size)", got)
	}
	for i := 0; i < 5; i++ {
		if err := c.Ensure(context.Background()); err != nil {
			t.Fatalf("Ensure %d: %v", i, err)
		}
	}
	if got := cs.submitCount(); got != 1 {
		t.Fatalf("acquisitions = %d, want no traffic while a slot is live", got)
	}
}

// A slot past the refresh age is skipped while a live slot exists: fresh
// cookies are preferred, but an aged one is still served when it is all
// there is (a request must never block on a solve).
func TestRingSkipsStaleSlots(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	var mu sync.Mutex
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	c := New(Config{BaseURL: cs.srv.URL, Pool: 2, Refresh: 45 * time.Minute, Now: clock, Logf: func(string, ...any) {}})
	if err := c.PrewarmNow(context.Background()); err != nil {
		t.Fatalf("PrewarmNow: %v", err)
	}
	// Age both past the margin, then make exactly one fresh.
	mu.Lock()
	now = now.Add(50 * time.Minute)
	mu.Unlock()
	c.mu.Lock()
	freshCookie := c.slots[0].cookie
	c.slots[0].acquired = c.now()
	c.mu.Unlock()

	for i := 0; i < 4; i++ {
		if _, cookie := c.Header(); cookie != CookieName+"="+freshCookie {
			t.Fatalf("request %d served %q, want only the fresh slot", i, cookie)
		}
	}

	// All stale: the ring still serves every slot round-robin.
	c.mu.Lock()
	c.slots[0].acquired = now.Add(-time.Hour)
	c.mu.Unlock()
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		_, cookie := c.Header()
		if cookie == "" {
			t.Fatal("a stale ring must still serve")
		}
		seen[cookie] = true
	}
	if len(seen) != 2 {
		t.Fatalf("stale ring served %d distinct cookies, want both", len(seen))
	}
}

// The refresh loop replaces one stale slot per tick: a three-slot ring does
// not burst three solves in one tick (SPEC §2.1.2).
func TestRingRefreshReplacesOneSlotPerTick(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	var mu sync.Mutex
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	c := New(Config{BaseURL: cs.srv.URL, Pool: 3, Refresh: time.Minute, Now: clock, Logf: func(string, ...any) {}})
	if err := c.PrewarmNow(context.Background()); err != nil {
		t.Fatalf("PrewarmNow: %v", err)
	}
	if got := cs.submitCount(); got != 3 {
		t.Fatalf("initial acquisitions = %d, want 3", got)
	}
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()

	if err := c.RefreshStale(context.Background()); err != nil {
		t.Fatalf("RefreshStale: %v", err)
	}
	if got := cs.submitCount(); got != 4 {
		t.Fatalf("acquisitions after one tick = %d, want exactly one replacement", got)
	}
	for i := 0; i < 2; i++ {
		if err := c.RefreshStale(context.Background()); err != nil {
			t.Fatalf("RefreshStale: %v", err)
		}
	}
	if got := cs.submitCount(); got != 6 {
		t.Fatalf("acquisitions after three ticks = %d, want all three slots replaced once", got)
	}
	st := c.State()
	for i, s := range st.Slots {
		if s.AgeSeconds > int64(time.Minute.Seconds()) {
			t.Fatalf("slot %d still stale after three ticks: %+v", i, st.Slots)
		}
	}
	// A fourth tick has nothing stale to do.
	if err := c.RefreshStale(context.Background()); err != nil {
		t.Fatalf("RefreshStale: %v", err)
	}
	if got := cs.submitCount(); got != 6 {
		t.Fatalf("acquisitions after a fourth tick = %d, want none", got)
	}
}

// A challenge retires only the slot that was used. The retired slot is
// refilled; the others keep their cookies and ages (SPEC §2.1.2).
func TestRingRetireTouchesOnlyThatSlot(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := New(Config{BaseURL: cs.srv.URL, Pool: 3, Logf: func(string, ...any) {}})
	if err := c.PrewarmNow(context.Background()); err != nil {
		t.Fatalf("PrewarmNow: %v", err)
	}
	_, used := c.Header()
	if err := c.Retire(used); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	st := c.State()
	if st.RefreshCount != 3 {
		t.Fatalf("refreshes = %d, want 3 (retire does not solve)", st.RefreshCount)
	}
	empty := 0
	for _, s := range st.Slots {
		if !s.Cookie {
			empty++
		}
	}
	if empty != 1 {
		t.Fatalf("empty slots = %d, want exactly the retired one: %+v", empty, st.Slots)
	}
	// A subsequent Header never hands out the retired value.
	for i := 0; i < 9; i++ {
		if _, cookie := c.Header(); cookie == used {
			t.Fatal("retired cookie was served again")
		}
	}
	// And the slot can be refilled, landing in the empty slot.
	if err := c.Refill(context.Background()); err != nil {
		t.Fatalf("Refill: %v", err)
	}
	if got := cs.submitCount(); got != 4 {
		t.Fatalf("acquisitions = %d, want one refill", got)
	}
	if st := c.State(); st.RefreshCount != 4 {
		t.Fatalf("refreshes = %d, want 4", st.RefreshCount)
	}
}

// Retiring a wrong value is a no-op, and a full ring with no empty slot still
// refills by replacing the oldest slot (the 429 replay path).
func TestRingRefillReplacesOldestWhenFull(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := New(Config{BaseURL: cs.srv.URL, Pool: 1, Logf: func(string, ...any) {}})
	if err := c.PrewarmNow(context.Background()); err != nil {
		t.Fatalf("PrewarmNow: %v", err)
	}
	_, before := c.Header()
	if err := c.Retire("_vcrcs=not-a-slot"); err != nil {
		t.Fatalf("Retire of an unknown value: %v", err)
	}
	if _, cookie := c.Header(); cookie != before {
		t.Fatalf("a no-op retire changed the ring: %q", cookie)
	}
	if err := c.Refill(context.Background()); err != nil {
		t.Fatalf("Refill: %v", err)
	}
	if _, cookie := c.Header(); cookie == before {
		t.Fatalf("refill on a full ring must replace the old cookie; still %q", cookie)
	}
}

// The ring-wide failed-acquisition cooldown survives retirement: a failed
// refill inside the window is reported, not retried.
func TestRingRefillRespectsCooldown(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	cs.failSubmit = 500
	now := time.Unix(1_800_000_000, 0)
	c := New(Config{BaseURL: cs.srv.URL, Pool: 2, Now: func() time.Time { return now }, Logf: func(string, ...any) {}})
	if err := c.Ensure(context.Background()); err == nil {
		t.Fatal("expected the first acquisition to fail")
	}
	if err := c.Refill(context.Background()); err == nil {
		t.Fatal("a refill inside the cooldown must report the failure, not retry")
	}
	if got := cs.submitCount(); got != 1 {
		t.Fatalf("submissions = %d, want 1 inside the cooldown", got)
	}
}

// An unchallenged egress holds no cookies; the ring records the probe once and
// does not manufacture slots (SPEC §2.1.1, L3).
func TestRingUnchallengedEgressHoldsNoSlots(t *testing.T) {
	t.Parallel()
	var probes atomic.Int32
	srv := newUnchallengedServer(t, &probes)
	c := New(Config{BaseURL: srv.URL, Pool: 3, Refresh: 45 * time.Minute, Logf: func(string, ...any) {}})
	if err := c.PrewarmNow(context.Background()); err != nil {
		t.Fatalf("PrewarmNow: %v", err)
	}
	st := c.State()
	if st.HasCookie || len(st.Slots) != 3 {
		t.Fatalf("state = %+v, want three empty slots and no cookie", st)
	}
	for i := 0; i < 5; i++ {
		if _, cookie := c.Header(); cookie != "" {
			t.Fatalf("unchallenged egress produced a cookie: %q", cookie)
		}
		if err := c.Ensure(context.Background()); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("probes = %d across boot + 5 requests, want 1", got)
	}
}

// Health reports one entry per configured slot even before any acquisition.
func TestRingStateHasOneEntryPerSlot(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := New(Config{BaseURL: cs.srv.URL, Pool: 4, Logf: func(string, ...any) {}})
	st := c.State()
	if len(st.Slots) != 4 {
		t.Fatalf("slots = %d, want 4", len(st.Slots))
	}
	for i, s := range st.Slots {
		if s.Cookie {
			t.Fatalf("slot %d has a cookie before any acquisition", i)
		}
	}
}

// Per-slot ages and refresh counts are reported individually; the top-level
// age is the oldest live slot and the top-level count is the total.
func TestRingStateReportsPerSlotAges(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	var mu sync.Mutex
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	c := New(Config{BaseURL: cs.srv.URL, Pool: 2, Refresh: 45 * time.Minute, Now: clock, Logf: func(string, ...any) {}})
	if err := c.PrewarmNow(context.Background()); err != nil {
		t.Fatalf("PrewarmNow: %v", err)
	}
	mu.Lock()
	now = now.Add(10 * time.Minute)
	mu.Unlock()
	st := c.State()
	if len(st.Slots) != 2 {
		t.Fatalf("slots = %d", len(st.Slots))
	}
	for i, s := range st.Slots {
		if !s.Cookie || s.AgeSeconds != 600 {
			t.Fatalf("slot %d = %+v, want a cookie aged 600 s", i, s)
		}
		if s.RefreshCount != 1 {
			t.Fatalf("slot %d refresh count = %d, want 1", i, s.RefreshCount)
		}
	}
	if !st.HasCookie || st.AgeSeconds != 600 || st.RefreshCount != 2 {
		t.Fatalf("top-level = %+v", st)
	}
}

// Concurrent Header calls must rotate without losing or duplicating slots.
func TestRingHeaderIsConcurrencySafe(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := New(Config{BaseURL: cs.srv.URL, Pool: 3, Logf: func(string, ...any) {}})
	if err := c.PrewarmNow(context.Background()); err != nil {
		t.Fatalf("PrewarmNow: %v", err)
	}
	const n = 300
	var wg sync.WaitGroup
	var empty atomic.Int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, cookie := c.Header(); cookie == "" {
				empty.Add(1)
			}
		}()
	}
	wg.Wait()
	if empty.Load() != 0 {
		t.Fatalf("%d concurrent Header calls saw no cookie", empty.Load())
	}
	if st := c.State(); st.RefreshCount != 3 {
		t.Fatalf("refreshes = %d, want no acquisitions under read load", st.RefreshCount)
	}
}
