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

// solvableToken crafts a solvable v2 challenge token: one round and a long
// enough target for the seed-derived key offset (seed 1000 → key 16 → the
// target slice is target[16:20]).
func solvableToken() string {
	payload := "id;pre;abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789;1;bin"
	return "2.1.1000." + base64.StdEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// newProbeServer records the headers of the GET /api/session probe and
// answers with the given status (+optional token header).
func newProbeServer(t *testing.T, status int, token string) (*httptest.Server, *http.Header) {
	t.Helper()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		if token != "" {
			w.Header().Set("x-vercel-challenge-token", token)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func newTestClearance(srv *httptest.Server) *Clearance {
	return New(Config{
		BaseURL:   srv.URL,
		Client:    srv.Client(),
		Timeout:   10 * time.Second,
		Cooldown:  -1,
		FillPace:  -1,
		Jitter:    -1,
		Pool:      2,
		Logf:      func(string, ...any) {},
	})
}

// The probe must look like a business API request, not a navigation. Vercel
// waves navigate-shaped requests through with a plain 200, which would
// falsely mark the egress as "no clearance needed" and leave the cookie pool
// empty forever.
func TestFetchChallengeProbeUsesAPIShapeHeaders(t *testing.T) {
	srv, got := newProbeServer(t, http.StatusOK, "")
	c := newTestClearance(srv)

	_, err := c.fetchChallenge(context.Background())
	if !errors.Is(err, errNoChallenge) {
		t.Fatalf("200 probe must yield errNoChallenge, got: %v", err)
	}

	for _, h := range []string{"Sec-Fetch-Mode", "Sec-Fetch-Dest", "Sec-Fetch-User", "Upgrade-Insecure-Requests"} {
		if v := got.Get(h); v != "" {
			t.Errorf("probe carried navigation header %s: %q", h, v)
		}
	}
	if ua := got.Get("User-Agent"); ua != UserAgent {
		t.Errorf("User-Agent = %q, want the shared clear.UserAgent", ua)
	}
	if acc := got.Get("Accept"); acc != "*/*" {
		t.Errorf("Accept = %q, want %q (API shape, not HTML)", acc, "*/*")
	}
	if strings.Contains(got.Get("Accept"), "text/html") {
		t.Errorf("Accept must not be the HTML navigation value, got %q", got.Get("Accept"))
	}
	if got.Get("Accept-Language") == "" {
		t.Errorf("Accept-Language must be set, matching business requests")
	}
}

func TestFetchChallenge200ReturnsNoChallenge(t *testing.T) {
	// Regression: an unchallenged probe (200) keeps the errNoChallenge path.
	srv, _ := newProbeServer(t, http.StatusOK, "")
	c := newTestClearance(srv)

	_, err := c.fetchChallenge(context.Background())
	if !errors.Is(err, errNoChallenge) {
		t.Fatalf("200 must return errNoChallenge, got: %v", err)
	}
}

func TestFetchChallenge429WithTokenReturnsToken(t *testing.T) {
	// Regression: 429 + challenge token still feeds the solve flow.
	srv, _ := newProbeServer(t, http.StatusTooManyRequests, solvableToken())
	c := newTestClearance(srv)

	token, err := c.fetchChallenge(context.Background())
	if err != nil {
		t.Fatalf("429 + token must return the token, got: %v", err)
	}
	if token == "" {
		t.Fatal("token must not be empty")
	}
	if _, err := solveToken(token); err != nil {
		t.Fatalf("returned token must be solvable: %v", err)
	}
}

func TestRefreshSolvesChallenge429AndFillsSlot(t *testing.T) {
	// End to end: probe gets 429 + token → solve → submit → slot goes live.
	var solutionSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/session":
			w.Header().Set("x-vercel-challenge-token", solvableToken())
			w.WriteHeader(http.StatusTooManyRequests)
		case r.Method == http.MethodPost && r.URL.Path == challengePath:
			solutionSeen = r.Header.Get("x-vercel-challenge-solution")
			http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "cookie-abc"})
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := newTestClearance(srv)

	if err := c.refresh(context.Background(), 0); err != nil {
		t.Fatalf("refresh must solve the challenge and fill the slot: %v", err)
	}
	if solutionSeen == "" {
		t.Fatal("solution must have been submitted to the challenge endpoint")
	}
	c.mu.Lock()
	cookie := c.slots[0].cookie
	c.mu.Unlock()
	if cookie == "" {
		t.Fatal("slot 0 must hold the acquired cookie after a solved challenge")
	}
}
