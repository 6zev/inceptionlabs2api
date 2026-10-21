package upstream

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// prewarmServer is a minimal challenge endpoint: it challenges until the
// _vcrcs cookie is presented, then answers /api/session with a valid token.
type prewarmServer struct {
	srv        *httptest.Server
	challenges int
}

func newPrewarmServer(t *testing.T) *prewarmServer {
	t.Helper()
	ps := &prewarmServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		if ck, err := r.Cookie("_vcrcs"); err == nil && ck.Value == "prewarmed" {
			io.WriteString(w, `{"ok":true,"token":"1792553057.`+strings.Repeat("0", 31)+`a.`+strings.Repeat("f", 64)+`"}`)
			return
		}
		ps.challenges++
		w.Header().Set("x-vercel-mitigated", "challenge")
		w.Header().Set("x-vercel-challenge-token", prewarmToken())
		w.WriteHeader(http.StatusTooManyRequests)
	})
	mux.HandleFunc("/.well-known/vercel/security/request-challenge", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "_vcrcs", Value: "prewarmed", Path: "/", MaxAge: 3600})
		w.WriteHeader(http.StatusNoContent)
	})
	ps.srv = httptest.NewServer(mux)
	t.Cleanup(ps.srv.Close)
	return ps
}

func prewarmToken() string {
	// A syntactically valid v2 token; the mock does not verify the proof.
	payload := strings.Join([]string{strings.Repeat("a", 32), "0d8769f6", strings.Repeat("0", 40), "3", "bin"}, ";")
	return "2.1792561477.60." + base64.StdEncoding.EncodeToString([]byte(payload)) + "." + strings.Repeat("0", 32)
}

func TestStartClearancePrewarmAcquiresWithoutRequests(t *testing.T) {
	t.Parallel()
	ps := newPrewarmServer(t)
	c, _ := NewClient(Config{BaseURL: ps.srv.URL, ClearanceFillPace: 5 * time.Millisecond,
		ClearanceJitter: -1, Logf: func(string, ...any) {}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Boot waits for exactly one slot (SPEC §2.1.2) and no longer. The bound
	// is generous because the solve is CPU-bound and this runs under -race.
	start := time.Now()
	c.StartClearancePrewarm(ctx)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("StartClearancePrewarm took %s, want a first-slot return", elapsed)
	}
	if st := c.Clearance().State(); st.Live != 1 {
		t.Fatalf("boot left %d live, want exactly 1", st.Live)
	}

	// The loop fills one slot at a time; wait for the ring to be full and
	// report which slots are up if it never is. Under -race each real solve
	// costs about a second, so 19 paced acquisitions need a longer deadline.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		st := c.Clearance().State()
		filled := 0
		for _, s := range st.Slots {
			if s.Cookie {
				filled++
			}
		}
		if filled == len(st.Slots) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ring not filled after prewarm: %+v", c.Clearance().State())
}

// The loop and prewarm run together as boot starts them; cancellation stops
// the loop and does not leave a goroutine behind.
func TestStartClearancePrewarmCancelsWithContext(t *testing.T) {
	t.Parallel()
	ps := newPrewarmServer(t)
	c, _ := NewClient(Config{BaseURL: ps.srv.URL, ClearanceFillPace: 5 * time.Millisecond,
		ClearanceJitter: -1, Logf: func(string, ...any) {}})
	ctx, cancel := context.WithCancel(context.Background())
	c.StartClearancePrewarm(ctx)
	cancel()
	// No assertion beyond the race detector and a prompt test exit: the loop
	// must observe cancellation.
	time.Sleep(50 * time.Millisecond)
}

// Boot fills the configured ring size, not just one cookie, and does so from
// the prewarm goroutine without any request traffic (SPEC §2.1.2).
func TestStartClearancePrewarmFillsTheRing(t *testing.T) {
	t.Parallel()
	ps := newPrewarmServer(t)
	c, _ := NewClient(Config{BaseURL: ps.srv.URL, ClearancePool: 2, ClearanceFillPace: 5 * time.Millisecond,
		ClearanceJitter: -1, Logf: func(string, ...any) {}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartClearancePrewarm(ctx)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st := c.Clearance().State()
		if len(st.Slots) == 2 && st.Slots[0].Cookie && st.Slots[1].Cookie {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ring not filled to its configured size: %+v", c.Clearance().State())
}
