package clear

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// challengeServer is a mock Vercel checkpoint endpoint. It issues a real v2
// token (so the solver runs for real), validates the submitted solution, and
// hands out a _vcrcs cookie.
type challengeServer struct {
	t        *testing.T
	srv      *httptest.Server
	tokens   atomic.Int32
	submits  atomic.Int32
	rejects  atomic.Int32
	mu       sync.Mutex
	solved   map[string]bool // challenge token -> solved
	issueTTL int
	// failSubmit forces POST request-challenge to answer with this status.
	failSubmit int
	// seedBase advances the seed per challenge so each token is distinct.
	seedBase int64
}

func newChallengeServer(t *testing.T) *challengeServer {
	t.Helper()
	c := &challengeServer{t: t, solved: map[string]bool{}, issueTTL: 3600, seedBase: 1792561400}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		if c.validCookie(r) {
			io.WriteString(w, `{"ok":true,"token":"1792553057.`+strings.Repeat("0", 31)+`a.`+strings.Repeat("f", 64)+`"}`)
			return
		}
		c.challenge(w)
	})
	mux.HandleFunc(challengePath, func(w http.ResponseWriter, r *http.Request) {
		c.submits.Add(1)
		if c.failSubmit != 0 {
			w.WriteHeader(c.failSubmit)
			return
		}
		token := r.Header.Get("x-vercel-challenge-token")
		solution := r.Header.Get("x-vercel-challenge-solution")
		if token == "" || solution == "" || r.Header.Get("x-vercel-challenge-version") != "2" {
			c.rejects.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if !verify(c.t, token, solution) {
			c.rejects.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		c.mu.Lock()
		c.solved[token] = true
		c.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "_vcrcs", Value: "1.cookievalue", Path: "/", MaxAge: 3600})
		w.WriteHeader(http.StatusNoContent)
	})
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

func (c *challengeServer) challenge(w http.ResponseWriter) {
	n := c.tokens.Add(1)
	seed := c.seedBase + int64(n)
	w.Header().Set("x-vercel-mitigated", "challenge")
	w.Header().Set("x-vercel-challenge-token", makeToken(seed))
	w.WriteHeader(http.StatusTooManyRequests)
}

// makeToken builds a syntactically valid v2 token whose payload is a real
// prefix/target/rounds tuple, so the solver has work to do.
func makeToken(seed int64) string {
	prefix := fmt.Sprintf("%08x", uint32(seed))
	target := strings.Repeat("0", 40) // any 40-hex target works; solver scans
	payload := strings.Join([]string{strings.Repeat("a", 32), prefix, target, "3", "bin"}, ";")
	raw := base64.StdEncoding.EncodeToString([]byte(payload))
	return fmt.Sprintf("2.%d.60.%s.%s", seed, raw, strings.Repeat("0", 32))
}

func (c *challengeServer) validCookie(r *http.Request) bool {
	ck, err := r.Cookie("_vcrcs")
	return err == nil && ck.Value == "1.cookievalue"
}

func (c *challengeServer) tokenCount() int  { return int(c.tokens.Load()) }
func (c *challengeServer) submitCount() int { return int(c.submits.Load()) }
func (c *challengeServer) rejectCount() int { return int(c.rejects.Load()) }

func newTestClearance(t *testing.T, srv *httptest.Server) *Clearance {
	t.Helper()
	return New(Config{
		BaseURL: srv.URL,
		Now:     time.Now,
		Logf:    func(string, ...any) {},
	})
}

func TestRefreshAcquiresCookie(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := newTestClearance(t, cs.srv)

	if _, cookie := c.Header(); cookie != "" {
		t.Fatalf("cookie before refresh = %q, want empty", cookie)
	}
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	ua, cookie := c.Header()
	if ua != UserAgent {
		t.Fatalf("ua = %q, want the shared Chrome UA", ua)
	}
	if cookie != "_vcrcs=1.cookievalue" {
		t.Fatalf("cookie = %q", cookie)
	}
	if got := c.Cookie(); got != "1.cookievalue" {
		t.Fatalf("Cookie() = %q", got)
	}
	if cs.tokenCount() != 1 || cs.submitCount() != 1 {
		t.Fatalf("tokens=%d submits=%d, want 1/1", cs.tokenCount(), cs.submitCount())
	}
	if cs.rejectCount() != 0 {
		t.Fatal("server rejected a solution our solver produced")
	}
	st := c.State()
	if !st.HasCookie || st.RefreshCount != 1 || st.AgeSeconds < 0 {
		t.Fatalf("state = %+v", st)
	}
}

func TestRefreshSendsTheSameUAItUses(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	// Assert the acquisition requests carry the UA the client will send.
	var got []string
	var mu sync.Mutex
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Header.Get("User-Agent"))
		mu.Unlock()
		cs.srv.Config.Handler.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	c := newTestClearance(t, proxy)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("no requests observed")
	}
	for _, ua := range got {
		if ua != UserAgent {
			t.Fatalf("acquisition UA = %q, want %q", ua, UserAgent)
		}
	}
}

func TestRefreshIsSingleFlight(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := newTestClearance(t, cs.srv)

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Refresh(context.Background()); err != nil {
				t.Errorf("Refresh: %v", err)
			}
		}()
	}
	wg.Wait()
	// Concurrent refreshes coalesce into one acquisition.
	if cs.tokenCount() != 1 {
		t.Fatalf("challenge fetches = %d, want 1 (single-flight)", cs.tokenCount())
	}
	if st := c.State(); st.RefreshCount != 1 {
		t.Fatalf("refresh count = %d, want 1", st.RefreshCount)
	}
}

func TestRefreshRespectsCooldown(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	now := time.Unix(1_800_000_000, 0)
	c := New(Config{BaseURL: cs.srv.URL, Now: func() time.Time { return now }, Logf: func(string, ...any) {}})

	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	// A second explicit refresh inside the cooldown is a no-op.
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if got := cs.tokenCount(); got != 1 {
		t.Fatalf("challenge fetches = %d, want 1 inside the cooldown", got)
	}
	now = now.Add(c.cfg.Cooldown)
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("third Refresh: %v", err)
	}
	if got := cs.tokenCount(); got != 2 {
		t.Fatalf("challenge fetches = %d, want 2 after the cooldown", got)
	}
}

func TestEnsureRefreshesAtSafetyMargin(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	now := time.Unix(1_800_000_000, 0)
	c := New(Config{
		BaseURL: cs.srv.URL,
		Refresh: 45 * time.Minute,
		Now:     func() time.Time { return now },
		Logf:    func(string, ...any) {},
	})

	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := cs.tokenCount(); got != 1 {
		t.Fatalf("challenge fetches = %d, want 1", got)
	}
	// Still fresh: Ensure reuses the cookie without network traffic.
	now = now.Add(44 * time.Minute)
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure fresh: %v", err)
	}
	if got := cs.tokenCount(); got != 1 {
		t.Fatalf("challenge fetches = %d at 44m, want 1", got)
	}
	// Past the 45m safety margin: Ensure refreshes proactively.
	now = now.Add(2 * time.Minute)
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure stale: %v", err)
	}
	if got := cs.tokenCount(); got != 2 {
		t.Fatalf("challenge fetches = %d at 46m, want 2 (proactive refresh)", got)
	}
	if st := c.State(); st.RefreshCount != 2 {
		t.Fatalf("refresh count = %d, want 2", st.RefreshCount)
	}
}

func TestEnsureReturnsErrorWithoutCookie(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	cs.failSubmit = http.StatusBadGateway
	c := newTestClearance(t, cs.srv)
	err := c.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "solution rejected") {
		t.Fatalf("err = %v, want a solution-rejected error", err)
	}
	if st := c.State(); st.HasCookie {
		t.Fatal("a failed acquisition must not report a cookie")
	}
}

func TestEnsureSurfaces403WithoutRetry(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	cs.failSubmit = http.StatusForbidden
	c := New(Config{BaseURL: cs.srv.URL, Cooldown: -1, Logf: func(string, ...any) {}})
	if err := c.Ensure(context.Background()); err == nil {
		t.Fatal("expected an error for a 403 challenge")
	}
	// 403 means the PoW was rejected; Ensure must not loop. A second call is a
	// second single acquisition, not a retry storm.
	if err := c.Ensure(context.Background()); err == nil {
		t.Fatal("expected an error on the second Ensure too")
	}
	if got := cs.submitCount(); got != 2 {
		t.Fatalf("submissions = %d for two Ensure calls, want 2 (no internal loop)", got)
	}
}

func TestInvalidateForcesNextEnsureToAcquire(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	now := time.Unix(1_800_000_000, 0)
	c := New(Config{BaseURL: cs.srv.URL, Cooldown: -1, Now: func() time.Time { return now }, Logf: func(string, ...any) {}})
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	c.Invalidate()
	if st := c.State(); st.HasCookie {
		t.Fatal("Invalidate left a cookie")
	}
	if _, cookie := c.Header(); cookie != "" {
		t.Fatal("Header still returns a cookie after Invalidate")
	}
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure after Invalidate: %v", err)
	}
	if got := cs.tokenCount(); got != 2 {
		t.Fatalf("challenge fetches = %d, want 2 (re-acquire)", got)
	}
}

func TestRefreshHTTPClientInterface(t *testing.T) {
	t.Parallel()
	// A Clearance must accept an injected client and transport so that
	// acquisition shares the caller's egress (proxy) and TLS roots.
	cs := newChallengeServer(t)
	c := New(Config{
		BaseURL: cs.srv.URL,
		Client:  cs.srv.Client(),
		Logf:    func(string, ...any) {},
	})
	if err := c.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh with injected client: %v", err)
	}
	if _, cookie := c.Header(); cookie == "" {
		t.Fatal("no cookie after refresh")
	}
}

func TestRefreshBadTokenIsReported(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == challengePath {
			t.Error("must not submit when the token is unusable")
			return
		}
		w.Header().Set("x-vercel-mitigated", "challenge")
		w.Header().Set("x-vercel-challenge-token", "garbage")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := newTestClearance(t, srv)
	if err := c.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "solve") {
		t.Fatalf("err = %v, want a solve error", err)
	}
}

// A 200 from the probe means the edge is not challenging this egress:
// acquisition is a successful no-op, not an error.
func TestRefreshNoChallengeIsSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true,"token":"1792553057.`+strings.Repeat("0", 31)+`a.`+strings.Repeat("f", 64)+`"}`)
	}))
	defer srv.Close()
	c := newTestClearance(t, srv)
	if err := c.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, cookie := c.Header(); cookie != "" {
		t.Fatalf("cookie = %q, want none when unchallenged", cookie)
	}
}

func TestRefreshOtherStatusIsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := newTestClearance(t, srv)
	if err := c.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "expected challenge") {
		t.Fatalf("err = %v, want an expected-challenge error", err)
	}
}

func TestRefreshNoCookieIsError(t *testing.T) {
	t.Parallel()
	token := makeToken(1792561401)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == challengePath {
			w.WriteHeader(http.StatusNoContent) // no Set-Cookie
			return
		}
		w.Header().Set("x-vercel-mitigated", "challenge")
		w.Header().Set("x-vercel-challenge-token", token)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := newTestClearance(t, srv)
	if err := c.Refresh(context.Background()); err == nil || !strings.Contains(err.Error(), "no _vcrc") {
		t.Fatalf("err = %v, want a missing-cookie error", err)
	}
}

func TestRefreshTimeout(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	defer close(block)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, Timeout: 100 * time.Millisecond, Logf: func(string, ...any) {}})
	start := time.Now()
	if err := c.Refresh(context.Background()); err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Refresh took %s, want a bounded timeout", elapsed)
	}
}

func TestConcurrentHeaderAndRefresh(t *testing.T) {
	t.Parallel()
	cs := newChallengeServer(t)
	c := New(Config{BaseURL: cs.srv.URL, Cooldown: -1, Logf: func(string, ...any) {}})
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.Header()
			c.State()
		}()
		go func() {
			defer wg.Done()
			_ = c.Refresh(context.Background())
		}()
	}
	wg.Wait()
	if _, cookie := c.Header(); cookie == "" {
		t.Fatal("no cookie after concurrent access")
	}
}

func TestUserAgentIsChrome(t *testing.T) {
	t.Parallel()
	if !strings.Contains(UserAgent, "Chrome/") {
		t.Fatalf("UserAgent = %q, want a Chrome UA (cookie binding)", UserAgent)
	}
}

// tlsServerForCert is exercised by the transport tests; keep the helper here
// so both test files can mint a local certificate.
func tlsServerForCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("load key pair: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append cert to pool")
	}
	return cert, pool
}
