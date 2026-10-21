package clear

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// A lazy Ensure reuses a held cookie inside the success cooldown, but a
// server-ordered replacement (Refresh after a 429) is a recovery path and is
// never blocked — the ring must not weaken the single-cookie rule that a
// challenge replay always gets a fresh solve.
func TestCooldownNeverBlocksAChallengeReplay(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	now := time.Unix(1_800_000_000, 0)
	c := New(Config{BaseURL: cs.srv.URL, Now: func() time.Time { return now }, Logf: func(string, ...any) {}})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	// Inside the cooldown a lazy request is served from a held slot.
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure inside the cooldown: %v", err)
	}
	if got := cs.submitCount(); got != 1 {
		t.Fatalf("submissions = %d, want 1 (lazy reuse)", got)
	}
	// A challenge replay must solve immediately, inside the same window.
	_, used := c.Header()
	if err := c.Retire(used); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh after a challenge: %v", err)
	}
	if got := cs.submitCount(); got != 2 {
		t.Fatalf("submissions = %d, want 2 (the replay is never cooldown-blocked)", got)
	}
}

// A failing checkpoint spaces every attempt, forced or lazy: inside a window
// opened by a failure both report the recorded error without solving.
func TestCooldownFromFailureSpacesForcedFills(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	cs.failSubmit = http.StatusBadGateway
	now := time.Unix(1_800_000_000, 0)
	c := New(Config{BaseURL: cs.srv.URL, Now: func() time.Time { return now }, Logf: func(string, ...any) {}})

	if err := c.Ensure(context.Background()); err == nil {
		t.Fatal("expected the first acquisition to fail")
	}
	if err := c.Refill(context.Background()); err == nil {
		t.Fatal("Refill inside a failure window must report the failure")
	}
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh inside a failure window must report the failure")
	}
	if got := cs.submitCount(); got != 1 {
		t.Fatalf("submissions = %d, want 1 inside the failure window", got)
	}
}

// A ring that is being filled by boot is not gated by the success cooldown of
// the slot it just filled: PrewarmNow must fill all slots back-to-back.
func TestCooldownDoesNotStallTheBootFill(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	now := time.Unix(1_800_000_000, 0)
	c := New(Config{BaseURL: cs.srv.URL, Pool: 3, Now: func() time.Time { return now }, Logf: func(string, ...any) {}})
	if err := c.PrewarmNow(context.Background()); err != nil {
		t.Fatalf("PrewarmNow: %v", err)
	}
	if got := cs.submitCount(); got != 3 {
		t.Fatalf("submissions = %d, want 3 (the fill is explicit work)", got)
	}
}
