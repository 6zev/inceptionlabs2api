package clear

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A refresh with a live slot present must re-solve THAT slot's challenge
// token, not mint a fresh one. A proof minted from a different challenge can
// be refused by the edge that issued the incumbent's, which shows up as a 429
// challenge that outlives a successful solve ("challenged even after a fresh
// clearance", one retired slot every ~15 s).
func TestRefreshReposTokenFromALiveSlot(t *testing.T) {
	var solvedTokens []string
	// A distinct but still solvable incumbent token: seed 1001 has the same
	// key offset, so only the token text differs.
	incumbent := "2.1.1001." + base64.StdEncoding.EncodeToString(
		[]byte("id;pre;abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789;1;bin")) + ".sig"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == challengePath {
			// Record which challenge token the client solved.
			solvedTokens = append(solvedTokens, r.Header.Get("x-vercel-challenge-token"))
			w.Header().Add("Set-Cookie", CookieName+"=c; Path=/; Max-Age=3600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("x-vercel-challenge-token", solvableToken())
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := newTestClearance(srv)
	c.cfg.Pool = 2
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// Pin the incumbent's token to something distinguishable and still inside
	// its own TTL (a live slot's token is only re-solved while it is younger
	// than tokenReuseWindow; past that the edge answers 703), then refresh.
	c.mu.Lock()
	c.slots[0].token = incumbent
	c.slots[0].tokenAt = time.Now()
	c.mu.Unlock()
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if len(solvedTokens) == 0 {
		t.Fatal("no solution was submitted")
	}
	last := solvedTokens[len(solvedTokens)-1]
	if last != incumbent {
		t.Fatalf("solved token %q, want the live slot's token (re-solve, not a fresh challenge)", last)
	}
}

// With no live slot (a cold ring) the fresh challenge token must be used: there
// is nothing to re-solve and no incumbent proof to prefer.
func TestRefreshUsesFreshTokenOnAColdRing(t *testing.T) {
	var solvedTokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == challengePath {
			solvedTokens = append(solvedTokens, r.Header.Get("x-vercel-challenge-token"))
			w.Header().Add("Set-Cookie", CookieName+"=c; Path=/; Max-Age=3600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("x-vercel-challenge-token", solvableToken())
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	c := newTestClearance(srv)
	c.cfg.Pool = 1
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(solvedTokens) != 1 {
		t.Fatalf("submitted %d solutions, want 1", len(solvedTokens))
	}
	if solvedTokens[0] != solvableToken() {
		t.Fatalf("solved token %q, want the probed challenge token", solvedTokens[0])
	}
}

// The token must not outlive the cookie it belongs to: a retired slot must not
// hand its token to a later refresh.
func TestRetireForgetsTheToken(t *testing.T) {
	srv := newExpiryServer(t, "3600")
	c := newTestClearance(srv)
	c.cfg.Pool = 1
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	c.mu.Lock()
	cookie, tok := c.slots[0].cookie, c.slots[0].token
	c.mu.Unlock()
	if cookie == "" || tok == "" {
		t.Fatal("slot has no cookie/token to retire")
	}
	if err := c.Retire(cookie); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	c.mu.Lock()
	gotTok := c.slots[0].token
	c.mu.Unlock()
	if gotTok != "" {
		t.Fatalf("retired slot kept its token %q", gotTok)
	}
	if _, ok := c.reposTokenLocked(time.Now()); ok {
		t.Fatal("reposTokenLocked returned a token after every slot was retired")
	}
}
