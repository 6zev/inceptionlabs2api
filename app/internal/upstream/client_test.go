package upstream

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
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

	"inceptionlabs2api/internal/clear"
	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/pool"
)

// localCA mints a certificate for 127.0.0.1 and returns the certificate and
// a pool that trusts it.
func localCA(t *testing.T) (tls.Certificate, *x509.CertPool) {
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
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	return cert, roots
}

// mockUpstream is an HTTPS server that impersonates /api/session and
// /api/chat.
type mockUpstream struct {
	t       *testing.T
	ln      net.Listener
	roots   *x509.CertPool
	baseURL string

	mu           sync.Mutex
	sessionCount int
	chatTokens   []string
	chatBodies   []string
	// sessionStatus overrides the /api/session response status/key.
	sessionStatus int
	sessionBody   string
	// chatHandler, when set, handles /api/chat; default emits one text block.
	chatHandler func(w http.ResponseWriter, r *http.Request)

	// checkpoint enables the Vercel checkpoint mock: an uncleared request to
	// /api/session gets a 429 challenge, and the solution route hands out a
	// _vcrcs cookie. It is on by default so every test exercises clearance.
	checkpoint bool
	// challengeTokens counts issued challenges; challengeSubmits counts
	// solution submissions and challengeRejects counts refused proofs.
	challengeTokens  atomic.Int32
	challengeSubmits atomic.Int32
	challengeRejects atomic.Int32
	// refuseSubmit forces the solution route to answer 403 (proof rejected).
	refuseSubmit bool

	// Search-lab fixtures. When the handlers are nil the routes answer 404.
	searchHandler   func(w http.ResponseWriter, r *http.Request)
	configHandler   func(w http.ResponseWriter, r *http.Request)
	searchTokens    []string
	searchBodies    []string
	searchEncodings []string

	// Decide fixtures. Handler nil means a canned 200.
	decideHandler func(w http.ResponseWriter, r *http.Request)
	decideTokens  []string
	decideAccepts []string
	decideBodies  [][]byte

	// Audio fixtures. Handlers nil means a canned 200.
	ttsHandler      func(w http.ResponseWriter, r *http.Request)
	sttHandler      func(w http.ResponseWriter, r *http.Request)
	ttsTokens       []string
	ttsBodies       []string
	sttTokens       []string
	sttContentTypes []string
	sttBodies       [][]byte
}

func startUpstream(t *testing.T) *mockUpstream {
	t.Helper()
	cert, roots := localCA(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"http/1.1"},
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	m := &mockUpstream{t: t, ln: ln, roots: roots, baseURL: "https://" + ln.Addr().String(), checkpoint: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", m.handleSession)
	mux.HandleFunc(challengePath, m.handleChallenge)
	mux.HandleFunc("/api/chat", m.handleChat)
	mux.HandleFunc("/api/search-lab/config", m.handleConfig)
	mux.HandleFunc("/api/search-lab/run", m.handleSearchRun)
	m.addAudioRoutes(mux)
	m.addDecideRoutes(mux)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return m
}

// challengePath mirrors the checkpoint's solution endpoint.
const challengePath = "/.well-known/vercel/security/request-challenge"

func (m *mockUpstream) handleSession(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.sessionCount++
	status, body, checkpoint := m.sessionStatus, m.sessionBody, m.checkpoint
	m.mu.Unlock()
	if r.Method != http.MethodGet {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		io.WriteString(w, body)
		return
	}
	if body != "" {
		io.WriteString(w, body)
		return
	}
	// The checkpoint challenges any request without clearance. Only the
	// ordinary (unset status/body) path challenges, so tests that script a
	// specific session response keep their meaning.
	if checkpoint {
		if ck, err := r.Cookie(clear.CookieName); err != nil || ck.Value != "cleared" {
			m.challenge(w)
			return
		}
	}
	writeSessionToken(w, m.sessionCount)
}

func (m *mockUpstream) challenge(w http.ResponseWriter) {
	n := m.challengeTokens.Add(1)
	w.Header().Set("x-vercel-mitigated", "challenge")
	w.Header().Set("x-vercel-challenge-token", checkpointToken(m.sessionCount+int(n)))
	w.WriteHeader(http.StatusTooManyRequests)
}

func (m *mockUpstream) handleChallenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	m.challengeSubmits.Add(1)
	if m.refuseSubmit {
		m.challengeRejects.Add(1)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	token := r.Header.Get("x-vercel-challenge-token")
	solution := r.Header.Get("x-vercel-challenge-solution")
	if token == "" || solution == "" || r.Header.Get("x-vercel-challenge-version") != "2" || !verifySolution(m.t, token, solution) {
		m.challengeRejects.Add(1)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: clear.CookieName, Value: "cleared", Path: "/", MaxAge: 3600})
	w.WriteHeader(http.StatusNoContent)
}

func writeSessionToken(w http.ResponseWriter, n int) {
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "token": writeSessionTokenValue(n)})
}

func writeSessionTokenValue(n int) string {
	// The signature must be hex (pool.ValidateToken): a 20-slot prewarm
	// already pushes the counter past the hex letters, so format it.
	sign := fmt.Sprintf("%064x", n)
	return "1792553057." + strings.Repeat("0", 31) + "a." + sign
}

func (m *mockUpstream) handleChat(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.chatTokens = append(m.chatTokens, r.Header.Get("x-session-token"))
	body, _ := io.ReadAll(r.Body)
	m.chatBodies = append(m.chatBodies, string(body))
	handler := m.chatHandler
	m.mu.Unlock()

	if handler != nil {
		handler(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("x-vercel-ai-ui-message-stream", "v1")
	io.WriteString(w, "data: {\"type\":\"reasoning-start\",\"id\":\"reasoning-0\"}\n\n")
	io.WriteString(w, "data: {\"type\":\"reasoning-end\",\"id\":\"reasoning-0\"}\n\n")
	io.WriteString(w, "data: {\"type\":\"text-start\",\"id\":\"text-0\"}\n\n")
	io.WriteString(w, "data: {\"type\":\"text-delta\",\"id\":\"text-0\",\"delta\":\"hi\"}\n\n")
	io.WriteString(w, "data: {\"type\":\"text-end\",\"id\":\"text-0\"}\n\n")
	io.WriteString(w, "data: [DONE]\n\n")
}

func (m *mockUpstream) handleConfig(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	h := m.configHandler
	m.mu.Unlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	h(w, r)
}

func (m *mockUpstream) handleSearchRun(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.searchTokens = append(m.searchTokens, r.Header.Get("x-session-token"))
	m.searchEncodings = append(m.searchEncodings, r.Header.Get("Accept-Encoding"))
	body, _ := io.ReadAll(r.Body)
	m.searchBodies = append(m.searchBodies, string(body))
	h := m.searchHandler
	m.mu.Unlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	h(w, r)
}

func (m *mockUpstream) tokens() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.chatTokens...)
}

func (m *mockUpstream) bodies() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.chatBodies...)
}

func (m *mockUpstream) searchRequests() (tokens, bodies, encodings []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.searchTokens...),
		append([]string(nil), m.searchBodies...),
		append([]string(nil), m.searchEncodings...)
}

func (m *mockUpstream) ttsRequests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.ttsBodies...)
}

type sttRequest struct {
	contentType string
	body        []byte
}

func (m *mockUpstream) sttRequests() []sttRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]sttRequest, 0, len(m.sttContentTypes))
	for i := range m.sttContentTypes {
		var body []byte
		if i < len(m.sttBodies) {
			body = m.sttBodies[i]
		}
		out = append(out, sttRequest{contentType: m.sttContentTypes[i], body: body})
	}
	return out
}

func (m *mockUpstream) sessions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionCount
}

func newTestClient(t *testing.T, m *mockUpstream, min int) (*Client, *pool.Pool) {
	t.Helper()
	c, fetch := NewClient(Config{
		BaseURL: m.baseURL,
		// The production default. Acquisition (solve + POST) runs inside this
		// context, so a short test value turns a slow CI solve into an
		// unrelated "clearance unavailable" failure.
		SessionTimeout: 15 * time.Second,
		RootCAs:        m.roots,
		Logf:           func(string, ...any) {},
	})
	p := pool.New(pool.Config{Min: min, Max: 8, TTL: time.Minute, Interval: time.Hour}, fetch)
	t.Cleanup(p.Close)
	c.pool = p
	return c, p
}

func TestFetchSession(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	c, _ := newTestClient(t, m, 0)
	tok, err := c.FetchSession(context.Background())
	if err != nil {
		t.Fatalf("FetchSession: %v", err)
	}
	if !pool.ValidateToken(tok) {
		t.Fatalf("token = %q, not valid shape", tok)
	}
}

// A checkpoint that never issues a token cannot be cleared: the fetch reports
// ErrClearance rather than a raw 429, and it does not retry.
func TestFetchSessionUnclearableChallenge(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.checkpoint = false
	m.sessionStatus = http.StatusTooManyRequests
	m.sessionBody = "<!DOCTYPE html><title>Vercel Security Checkpoint</title>"
	c, _ := newTestClient(t, m, 0)
	_, err := c.FetchSession(context.Background())
	if !errors.Is(err, ErrClearance) {
		t.Fatalf("err = %v, want ErrClearance", err)
	}
	if got := m.challengeSubmits.Load(); got != 0 {
		t.Fatalf("submissions = %d, want 0", got)
	}
}

func TestFetchSessionRejectsBadPayload(t *testing.T) {
	t.Parallel()
	cases := []string{
		`{"ok":false,"token":""}`,
		`{"ok":true,"token":"short"}`,
		`not json`,
		`{"ok":true,"token":"1792553057.zzz.abc"}`,
	}
	for _, body := range cases {
		m := startUpstream(t)
		m.sessionBody = body
		c, _ := newTestClient(t, m, 0)
		if _, err := c.FetchSession(context.Background()); err == nil {
			t.Errorf("body %q: expected error", body)
		}
	}
}

func TestChatSendsTokenAndBody(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	c, _ := newTestClient(t, m, 2)
	req := NewChatRequest([]openai.TextMessage{openaiText("hello")}, "high")
	stream, err := c.Chat(context.Background(), req, time.Second, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()
	var got []Event
	if err := stream.Reader.Read(context.Background(), func(ev Event) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("events = %d, want 5", len(got))
	}
	tokens := m.tokens()
	if len(tokens) != 1 || !pool.ValidateToken(tokens[0]) {
		t.Fatalf("tokens = %v", tokens)
	}
	var body ChatRequest
	if err := json.Unmarshal([]byte(m.bodies()[0]), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.ReasoningEffort != "high" || !body.WebSearchEnabled {
		t.Fatalf("body = %+v", body)
	}
	if len(body.Messages) != 1 || body.Messages[0].Parts[0].Text != "hello" {
		t.Fatalf("messages = %+v", body.Messages)
	}
}

func TestChatRetriesOn401WithFreshToken(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	var attempts atomic.Int32
	m.chatHandler = func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"text-delta\",\"id\":\"text-0\",\"delta\":\"ok\"}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}
	c, p := newTestClient(t, m, 2)
	stream, err := c.Chat(context.Background(), NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), time.Second, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
	tokens := m.tokens()
	if len(tokens) != 2 || tokens[0] == tokens[1] {
		t.Fatalf("tokens = %v, want two distinct attempts", tokens)
	}
	// The rejected token is gone and a replacement was fetched.
	deadline := time.Now().Add(2 * time.Second)
	for p.Stats().Size < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st := p.Stats(); st.Size != 2 {
		t.Fatalf("pool = %+v, want refilled to 2", st)
	}
}

func TestChatGivesUpAfterTwoRetries(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	var attempts atomic.Int32
	m.chatHandler = func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}
	c, _ := newTestClient(t, m, 1)
	_, err := c.Chat(context.Background(), NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), time.Second, nil)
	if !errors.Is(err, ErrAuthRejected) {
		t.Fatalf("err = %v, want ErrAuthRejected", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3 (initial + 2 retries)", attempts.Load())
	}
}

func TestChatNon200IsTypedError(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.chatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "<html>internal</html>")
	}
	c, p := newTestClient(t, m, 1)
	_, err := c.Chat(context.Background(), NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), time.Second, nil)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusInternalServerError {
		t.Fatalf("err = %v, want StatusError 500", err)
	}
	// A 5xx does not discard the token: the pool still has one.
	if st := p.Stats(); st.Size != 1 {
		t.Fatalf("pool = %+v, want size 1", st)
	}
}

func TestChatEmptyPoolIsUnavailable(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.sessionBody = `{"ok":true,"token":"short"}` // a non-clearance failure
	c, _ := newTestClient(t, m, 0)
	_, err := c.Chat(context.Background(), NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), time.Second, nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestChatClientCancelKeepsToken(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	started := make(chan struct{})
	m.chatHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"text-delta\",\"id\":\"text-0\",\"delta\":\"a\"}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}
	c, p := newTestClient(t, m, 1)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := c.Chat(ctx, NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), time.Second, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	readErr := make(chan error, 1)
	go func() {
		readErr <- stream.Reader.Read(ctx, func(Event) error { return nil })
	}()
	<-started
	cancel()
	stream.Close()
	select {
	case err := <-readErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read did not return after cancel")
	}
	// The token must still be in the pool: a disconnect is not a rejection.
	if st := p.Stats(); st.Size == 0 {
		t.Fatal("pool lost the token on client cancel")
	}
}

// openaiText builds a minimal text message.
func openaiText(text string) openai.TextMessage {
	return openai.TextMessage{Role: "user", Texts: []string{text}}
}

func TestStatusErrorString(t *testing.T) {
	t.Parallel()
	err := &StatusError{Status: 429, Body: "  challenge  "}
	if got := err.Error(); !strings.Contains(got, "429") || !strings.Contains(got, "challenge") {
		t.Fatalf("Error() = %q", got)
	}
	long := &StatusError{Status: 500, Body: strings.Repeat("x", 500)}
	if got := long.Error(); len(got) > 300 {
		t.Fatalf("Error() should truncate, len = %d", len(got))
	}
}

func TestNewClientAttachesPoolAndBindPool(t *testing.T) {
	t.Parallel()
	p := pool.New(pool.Config{Min: 0, Max: 1, TTL: time.Minute}, func(context.Context) (string, error) {
		return writeSessionTokenValue(1), nil
	})
	defer p.Close()
	// The pool is injectable at construction (the startup path) or afterwards
	// (tests that must build the fetcher first).
	c, _ := NewClient(Config{BaseURL: "https://x", Pool: p})
	if c.pool != p {
		t.Fatal("Config.Pool was not attached")
	}
	c2, _ := NewClient(Config{BaseURL: "https://x"})
	c2.BindPool(p)
	if c2.pool != p {
		t.Fatal("BindPool did not attach the pool")
	}
}

func TestChatHeaderTimeoutIsBounded(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	block := make(chan struct{})
	defer close(block)
	m.chatHandler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}
	c, p := newTestClient(t, m, 1)
	// Warm the pool so the bound being measured is the header phase, not the
	// one-off clearance solve.
	if _, err := p.Get(context.Background()); err != nil {
		t.Fatalf("warm pool: %v", err)
	}
	start := time.Now()
	_, err := c.Chat(context.Background(), NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), 200*time.Millisecond, nil)
	// The header bound expiring is reported as the idle timeout (504 on the
	// server), never as an auth rejection or a silent success.
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("waited %s for headers, want the idle bound", elapsed)
	}
}

func TestRedirectPolicy(t *testing.T) {
	t.Parallel()
	mk := func(raw string) *http.Request {
		req, _ := http.NewRequest(http.MethodPost, raw, nil)
		return req
	}
	orig := mk("https://chat.inceptionlabs.ai/api/chat")

	if err := checkRedirect(mk("https://chat.inceptionlabs.ai/api/other"), []*http.Request{orig}); err != nil {
		t.Fatalf("same-host redirect rejected: %v", err)
	}
	if err := checkRedirect(mk("https://evil.example/api/chat"), []*http.Request{orig}); err == nil {
		t.Fatal("cross-host redirect allowed; the session token would leak")
	}
	if err := checkRedirect(mk("http://chat.inceptionlabs.ai/api/chat"), []*http.Request{orig}); err == nil {
		t.Fatal("HTTPS downgrade redirect allowed")
	}
	if err := checkRedirect(orig, []*http.Request{orig, orig, orig}); err == nil {
		t.Fatal("redirect loop not stopped")
	}
}

func TestChatDoesNotFollowCrossHostRedirect(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	var leaked atomic.Int32
	evil := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-session-token") != "" {
			leaked.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer evil.Close()
	m.chatHandler = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/api/chat", http.StatusFound)
	}
	c, _ := newTestClient(t, m, 1)
	_, err := c.Chat(context.Background(), NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), time.Second, nil)
	if err == nil {
		t.Fatal("cross-host redirect was followed")
	}
	if leaked.Load() != 0 {
		t.Fatal("session token leaked to the redirect target")
	}
}

// TestChatHeaderPhaseCoversDial blocks TLS handshakes entirely (the listener
// accepts TCP but never responds) to prove the pre-header phase is bounded
// even though ResponseHeaderTimeout alone only covers an established conn.
func TestChatHeaderPhaseCoversDial(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer silent.Close()
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		for _, c := range held {
			c.Close()
		}
		mu.Unlock()
	}()

	// Chat's header bound covers the whole pre-response phase, including the
	// TCP connect and TLS handshake that ResponseHeaderTimeout ignores.
	c, p := newTestClient(t, m, 1)
	// Warm the pool against the working mock before pointing the client at
	// the silent listener (the pool fetcher uses the same base URL).
	if _, err := p.Get(context.Background()); err != nil {
		t.Fatalf("warm pool: %v", err)
	}
	c.base = "https://" + silent.Addr().String()
	start := time.Now()
	_, err = c.do(context.Background(), http.MethodPost, "/api/chat", []byte(`{}`), 200*time.Millisecond, func(r *http.Request) {
		r.Header.Set("x-session-token", writeSessionTokenValue(1))
	})
	if err == nil {
		t.Fatal("expected a pre-header timeout")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("post hung for %s on a silent TLS peer", elapsed)
	}
	start = time.Now()
	_, err = c.Chat(context.Background(), NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), 200*time.Millisecond, nil)
	if err == nil {
		t.Fatal("expected a pre-header timeout")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Chat hung for %s on a silent TLS peer", elapsed)
	}
}

// TestChatReacquiresClearanceOn429 proves the retry contract: a request that
// was challenged despite carrying a cookie triggers exactly one refresh and
// one replay, and the replay succeeds.
func TestChatReacquiresClearanceOn429(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	var attempts atomic.Int32
	m.chatHandler = func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			// A challenge even though clearance was acquired.
			m.challenge(w)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"text-delta\",\"id\":\"text-0\",\"delta\":\"ok\"}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}
	c, _ := newTestClient(t, m, 1)
	stream, err := c.Chat(context.Background(), NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), time.Second, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()
	io.Copy(io.Discard, stream.Body)
	if attempts.Load() != 2 {
		t.Fatalf("chat attempts = %d, want 2 (challenge + replay)", attempts.Load())
	}
	// One acquisition before the first attempt, one refresh after the 429.
	if got := m.challengeSubmits.Load(); got != 2 {
		t.Fatalf("clearance acquisitions = %d, want 2", got)
	}
	if got := m.challengeRejects.Load(); got != 0 {
		t.Fatalf("solver produced %d rejected proofs", got)
	}
}

// TestChallengeIsRetriedOnlyOnce: a persistent 429 must not become a loop.
func TestChallengeIsRetriedOnlyOnce(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	var attempts atomic.Int32
	m.chatHandler = func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		m.challenge(w)
	}
	c, _ := newTestClient(t, m, 1)
	_, err := c.Chat(context.Background(), NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), time.Second, nil)
	// A challenge that survives a fresh clearance is reported as a clearance
	// failure, not handed back as an ordinary 429 body.
	if !errors.Is(err, ErrClearance) {
		t.Fatalf("err = %v, want ErrClearance", err)
	}
	// One attempt plus exactly one replay; postAuthed retries 401/403 only,
	// so a persistent 429 ends the request rather than looping.
	if attempts.Load() != 2 {
		t.Fatalf("chat attempts = %d, want 2 (no challenge loop)", attempts.Load())
	}
}

// TestSessionAndChatShareOneClearance: the pool fetcher and chat use the same
// Clearance, so only one solve happens for a warmed pool plus one chat.
func TestSessionAndChatShareOneClearance(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	c, _ := newTestClient(t, m, 2)
	stream, err := c.Chat(context.Background(), NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), time.Second, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	stream.Close()
	if got := m.challengeSubmits.Load(); got != 1 {
		t.Fatalf("clearance acquisitions = %d, want 1 shared across the pool and chat", got)
	}
	state := c.Clearance().State()
	if !state.HasCookie || state.RefreshCount != 1 {
		t.Fatalf("clearance state = %+v", state)
	}
}

// TestClearanceFailureIsTyped: when the checkpoint refuses every proof, chat
// fails with ErrClearance instead of hammering the upstream.
func TestClearanceFailureIsTyped(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.refuseSubmit = true
	c, _ := newTestClient(t, m, 0)
	_, err := c.Chat(context.Background(), NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), time.Second, nil)
	if !errors.Is(err, ErrClearance) {
		t.Fatalf("err = %v, want ErrClearance", err)
	}
	// The pool reports the cause, but the clearance failure stays first in
	// the chain so the server can map it to upstream_clearance_failed.
	if errors.Is(err, ErrAuthRejected) {
		t.Fatalf("clearance failure misclassified as auth: %v", err)
	}
}

// TestEveryRequestCarriesUAAndCookie checks the binding invariants: one UA
// constant, and the cookie on requests once held.
func TestEveryRequestCarriesUAAndCookie(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	var mu sync.Mutex
	var uas, cookies []string
	// Observe via the search route, which postAuthed also funnels through.
	m.searchHandler = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		uas = append(uas, r.Header.Get("User-Agent"))
		cookies = append(cookies, r.Header.Get("Cookie"))
		mu.Unlock()
		io.WriteString(w, `{"t":"docs","sources":[]}`+"\n")
	}
	c, _ := newTestClient(t, m, 1)
	if _, err := c.Search(context.Background(), &SearchRequest{Query: "q"}, time.Second); err != nil {
		t.Fatalf("Search: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(uas) != 1 {
		t.Fatalf("requests observed = %d", len(uas))
	}
	if uas[0] != clear.UserAgent {
		t.Fatalf("UA = %q, want the clearance UA", uas[0])
	}
	if cookies[0] != clear.CookieName+"=cleared" {
		t.Fatalf("cookie = %q, want the clearance cookie", cookies[0])
	}
}

func TestStatusErrorMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want string
	}{
		{"json envelope", `{"error":{"message":"model overloaded"}}`, "model overloaded"},
		{"flat json", `{"message":"bad input"}`, "bad input"},
		{"plain text", "upstream said no", "upstream said no"},
		{"html challenge", "<!DOCTYPE html><title>Vercel Security Checkpoint</title>", ""},
		{"empty", "   ", ""},
		{"unparsed json", `{"error":`, `{"error":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := (&StatusError{Status: 500, Body: tc.body}).Message(); got != tc.want {
				t.Fatalf("Message() = %q, want %q", got, tc.want)
			}
		})
	}
	long := strings.Repeat("x", 5000)
	got := (&StatusError{Status: 500, Body: long}).Message()
	if !strings.HasPrefix(got, long[:maxClientMessage]) || len(got) > maxClientMessage+3 {
		t.Fatalf("long message len = %d, want a bounded prefix of %d", len(got), maxClientMessage)
	}
}

// A challenge retires the slot it was carried by and replaces it; the ring is
// never invalidated wholesale (SPEC §2.1.2).
func TestChallengeRetiresTheSlotAndRefills(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	var attempts atomic.Int32
	m.chatHandler = func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			m.challenge(w)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"text-delta\",\"id\":\"text-0\",\"delta\":\"ok\"}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}
	c, _ := newTestClient(t, m, 1)
	if err := c.Clearance().Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	live := func() int {
		n := 0
		for _, s := range c.Clearance().State().Slots {
			if s.Cookie {
				n++
			}
		}
		return n
	}
	// One slot is held; a challenge must not clear others or leave the ring
	// short. (With one held slot the retirement is observable as a gap that
	// the lazy Ensure performs when there is no other slot to replay on.)
	if got := live(); got != 1 {
		t.Fatalf("live slots before = %d, want 1", got)
	}
	stream, err := c.Chat(context.Background(), NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), time.Second, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	stream.Close()
	if got := m.challengeSubmits.Load(); got < 2 {
		t.Fatalf("clearance acquisitions = %d, want the challenge replay to solve", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if live() >= 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("ring is empty after the challenge: %+v", c.Clearance().State())
}

// The config probe goes through the same token wrapper as every family: a
// first 401 is retried with a fresh token, not surfaced.
func TestSearchConfigRetriesOn401(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	var calls atomic.Int32
	m.configHandler = func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, `{"retrievalAvailable":true}`)
	}
	c, _ := newTestClient(t, m, 1)
	cfg, err := c.SearchConfig(context.Background())
	if err != nil {
		t.Fatalf("SearchConfig: %v", err)
	}
	if !cfg.RetrievalAvailable {
		t.Fatal("probe did not report availability")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("probe attempts = %d, want 2 (401 then retry)", got)
	}
}

// A 429 retires its slot and the replay goes out immediately on another live
// slot: the request path never waits for a solve (SPEC §2.1.2). The fill loop
// is stopped and its slot refill is not triggered, so the only way the replay
// can succeed is by rotating to a slot that is already live.
func TestChallengeReplayDoesNotWaitForASolve(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	var attempts atomic.Int32
	m.chatHandler = func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			m.challenge(w)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"text-delta\",\"id\":\"text-0\",\"delta\":\"ok\"}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}
	c, _ := newTestClient(t, m, 1)
	// The fill loop is NOT started: no background refill can be happening.
	// A full ring is filled synchronously, so a live slot exists to replay on.
	if err := c.Clearance().PrewarmNow(context.Background()); err != nil {
		t.Fatalf("PrewarmNow: %v", err)
	}
	// Warm the token pool while the ring is live: once a slot is retired, a
	// fetch made with no clearance would challenge the session route.
	if _, err := c.FetchSession(context.Background()); err != nil {
		t.Fatalf("FetchSession: %v", err)
	}
	submitsBefore := m.challengeSubmits.Load()

	stream, err := c.Chat(context.Background(), NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), time.Second, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	stream.Close()
	if got := m.challengeSubmits.Load(); got != submitsBefore {
		t.Fatalf("clearance submissions = %d during the replay, want %d (no inline solve)", got, submitsBefore)
	}
}
