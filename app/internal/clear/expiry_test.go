package clear

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newExpiryServer answers the challenge probe and the submission, giving the
// cookie the given Max-Age. It is the smallest end-to-end clearance the
// submitSolution deadline depends on.
func newExpiryServer(t *testing.T, maxAge string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case challengePath:
			w.Header().Add("Set-Cookie", CookieName+"=cookie-under-test; Path=/; HttpOnly; Max-Age="+maxAge)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Header().Set("x-vercel-challenge-token", solvableToken())
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The slot's deadline must come from the edge's own Max-Age. Serving a cookie
// past it is what produced the observed wave of 401s every ~28 minutes on a
// long-running gateway, while the ring still reported every slot live.
func TestRefreshRecordsTheEdgeMaxAge(t *testing.T) {
	srv := newExpiryServer(t, "3600")
	c := newTestClearance(srv)
	c.cfg.Pool = 1

	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	c.mu.Lock()
	s := c.slots[0]
	c.mu.Unlock()
	if s.expiresAt.IsZero() {
		t.Fatal("expiresAt is zero; Max-Age was not recorded")
	}
	if got := s.expiresAt.Sub(s.acquired); got < 3599*time.Second || got > 3600*time.Second {
		t.Fatalf("expiresAt-acquired = %v, want ~3600s", got)
	}
}

// An expired cookie must never be served: Header falls through to "" so the
// request takes the challenge path instead of a 401.
func TestHeaderNeverServesAnExpiredCookie(t *testing.T) {
	srv := newExpiryServer(t, "1")
	c := newTestClearance(srv)
	c.cfg.Pool = 1
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Freeze time past the Max-Age.
	now := c.now()
	c.now = func() time.Time { return now.Add(2 * time.Second) }

	if _, cookie := c.Header(); cookie != "" {
		t.Fatalf("Header served an expired cookie: %q", cookie)
	}
	st := c.State()
	if st.Live != 0 || st.HasCookie {
		t.Fatalf("State reports live=%d has_cookie=%v for an expired slot", st.Live, st.HasCookie)
	}
	if st.Slots[0].State != SlotPending {
		t.Fatalf("expired slot state = %q, want %q", st.Slots[0].State, SlotPending)
	}
	if st.Slots[0].RemainingSeconds == nil || *st.Slots[0].RemainingSeconds >= 0 {
		t.Fatalf("remaining_seconds must be present and negative, got %v", st.Slots[0].RemainingSeconds)
	}

	// The fill loop must treat the dead cookie as work to do, not as a
	// healthy partial ring.
	c.mu.Lock()
	idx := c.fillTargetLocked(c.now())
	c.mu.Unlock()
	if idx != 0 {
		t.Fatalf("fillTargetLocked = %d for an expired slot, want 0", idx)
	}
}

// A cookie whose response carried no deadline keeps working on the Refresh
// margin alone (zero expiresAt must not be read as "expired in 1970").
func TestNoMaxAgeKeepsTheCookieServed(t *testing.T) {
	// A response with no Max-Age at all needs its own handler.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == challengePath {
			w.Header().Add("Set-Cookie", CookieName+"=no-deadline; Path=/")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("x-vercel-challenge-token", solvableToken())
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv2.Close)

	c2 := newTestClearance(srv2)
	c2.cfg.Pool = 1
	if err := c2.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, cookie := c2.Header(); cookie == "" {
		t.Fatal("a cookie without Max-Age was not served")
	}
}
