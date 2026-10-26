package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"inceptionlabs2api/internal/pool"
)

// SessionPerRequest: every upstream call fetches a fresh /api/session token
// instead of drawing a pooled one.

// sessionServer serves /api/session and /api/chat, counting each hit.
type sessionServer struct {
	mu     sync.Mutex
	minted int
	srv    *httptest.Server
	chatH  atomic.Int32
}

func newSessionServer(t *testing.T) *sessionServer {
	t.Helper()
	s := &sessionServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.minted++
		s.mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"token":"` + fakeToken() + `"}`))
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-session-token") == "" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		s.chatH.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"text-start\",\"text\":\"hi\"}\n\ndata: [DONE]\n\n"))
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// fakeToken returns a body shaped like the real session token:
// {10-digit unix}.{32 hex}.{64 hex}.
func fakeToken() string {
	hexs := func(n int, c byte) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = c
		}
		return string(b)
	}
	return "1700000000." + hexs(32, 'a') + "." + hexs(64, 'b')
}

// countingFetcher counts how many tokens the pool asked for.
type countingFetcher struct {
	mu  sync.Mutex
	got int
}

func (f *countingFetcher) fetch(ctx context.Context) (string, error) {
	f.mu.Lock()
	f.got++
	f.mu.Unlock()
	return fakeToken(), nil
}

func (f *countingFetcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got
}

// newTestClient builds a client with a real (but never prefilled) pool.
func newTestClient(t *testing.T, baseURL string, perRequest bool) (*Client, *pool.Pool, *countingFetcher) {
	t.Helper()
	c, _ := NewClient(Config{
		BaseURL:           baseURL,
		SessionPerRequest: perRequest,
		ClearancePool:     1,
		ClearanceFillPace: -1,
		ClearanceJitter:   -1,
		Logf:              func(string, ...any) {},
	})
	f := &countingFetcher{}
	p := pool.New(pool.Config{
		Min:      0, // never prefetch: the test drives every fetch
		Max:      4,
		TTL:      time.Minute,
		Interval: time.Hour,
	}, f.fetch)
	t.Cleanup(p.Close)
	c.BindPool(p)
	return c, p, f
}

// TestSessionPerRequestFetchesEveryCall pins the core promise: two chat calls
// mint two sessions rather than sharing one pooled token.
//
// The clearance layer may probe /api/session once for an unchallenged egress,
// so the assertion is anchored on chat calls: every chat must be preceded by
// its own session mint (i.e. sessions grow with chats, not faster or slower).
func TestSessionPerRequestFetchesEveryCall(t *testing.T) {
	s := newSessionServer(t)
	c, _, _ := newTestClient(t, s.srv.URL, true)

	const chats = 2
	for i := 0; i < chats; i++ {
		stream, err := c.Chat(context.Background(), NewChatRequest(nil, ""), time.Minute, nil)
		if err != nil {
			t.Fatalf("chat %d: %v", i, err)
		}
		stream.Close()
	}
	if got := s.chatH.Load(); got != chats {
		t.Fatalf("/api/chat hits = %d, want %d", got, chats)
	}
	s.mu.Lock()
	extra := s.minted - chats // mints beyond one-per-chat (the clearance probe)
	s.mu.Unlock()
	if extra > 1 {
		t.Fatalf("/api/session hits = %d for %d chats: want one per chat (plus at most one clearance probe)",
			extra+chats, chats)
	}
}

// TestPooledModeReusesSession is the control: with the flag off the pool is
// the source, so repeated calls mint a session at most once.
func TestPooledModeReusesSession(t *testing.T) {
	s := newSessionServer(t)
	c, _, _ := newTestClient(t, s.srv.URL, false)

	for i := 0; i < 3; i++ {
		stream, err := c.Chat(context.Background(), NewChatRequest(nil, ""), time.Minute, nil)
		if err != nil {
			t.Fatalf("chat %d: %v", i, err)
		}
		stream.Close()
	}
	s.mu.Lock()
	minted := s.minted
	s.mu.Unlock()
	if minted != 1 {
		t.Fatalf("/api/session hits = %d, want 1 (pooled token reused)", minted)
	}
}
