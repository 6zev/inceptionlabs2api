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

	// Search-lab fixtures. When the handlers are nil the routes answer 404.
	searchHandler  func(w http.ResponseWriter, r *http.Request)
	configHandler  func(w http.ResponseWriter, r *http.Request)
	searchTokens   []string
	searchBodies   []string
	searchEncodings []string
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
	m := &mockUpstream{t: t, ln: ln, roots: roots, baseURL: "https://" + ln.Addr().String()}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", m.handleSession)
	mux.HandleFunc("/api/chat", m.handleChat)
	mux.HandleFunc("/api/search-lab/config", m.handleConfig)
	mux.HandleFunc("/api/search-lab/run", m.handleSearchRun)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return m
}

func (m *mockUpstream) handleSession(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.sessionCount++
	status, body := m.sessionStatus, m.sessionBody
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
	writeSessionToken(w, m.sessionCount)
}

func writeSessionToken(w http.ResponseWriter, n int) {
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "token": writeSessionTokenValue(n)})
}

func writeSessionTokenValue(n int) string {
	sign := strings.Repeat(string(rune('a'+(n%26))), 64)
	return "1792553057." + strings.Repeat("0", 31) + string(rune('a'+(n%26))) + "." + sign
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

func (m *mockUpstream) sessions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionCount
}

func newTestClient(t *testing.T, m *mockUpstream, min int) (*Client, *pool.Pool) {
	t.Helper()
	c, fetch := NewClient(Config{
		BaseURL:        m.baseURL,
		SessionTimeout: 5 * time.Second,
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

func TestFetchSessionRejectsChallenge(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.sessionStatus = http.StatusTooManyRequests
	m.sessionBody = "<!DOCTYPE html><title>Vercel Security Checkpoint</title>"
	c, _ := newTestClient(t, m, 0)
	_, err := c.FetchSession(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusTooManyRequests {
		t.Fatalf("err = %v, want StatusError 429", err)
	}
	if !strings.Contains(se.Body, "Checkpoint") {
		t.Fatalf("body = %q", se.Body)
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
	if body.ReasoningEffort != "high" || body.WebSearchEnabled {
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
	m.sessionStatus = http.StatusTooManyRequests
	m.sessionBody = "challenge"
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
	tok := m.tokens()[0]
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
	_ = tok
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

func TestClientLogfAndBindPool(t *testing.T) {
	t.Parallel()
	c, _ := NewClient(Config{BaseURL: "https://x"})
	if c.Logf() == nil {
		t.Fatal("Logf() = nil")
	}
	p := pool.New(pool.Config{Min: 0, Max: 1, TTL: time.Minute}, func(context.Context) (string, error) {
		return writeSessionTokenValue(1), nil
	})
	defer p.Close()
	c.BindPool(p)
	if c.pool != p {
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
	c, _ := newTestClient(t, m, 1)
	start := time.Now()
	_, err := c.Chat(context.Background(), NewChatRequest([]openai.TextMessage{openaiText("hi")}, ""), 100*time.Millisecond, nil)
	var se *StatusError
	if err == nil || errors.As(err, &se) {
		// The client wraps the transport error; a timeout is the expected
		// outcome and must not look like an auth rejection.
		if errors.Is(err, ErrAuthRejected) {
			t.Fatalf("header timeout misclassified as auth rejection: %v", err)
		}
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
	_, err = c.post(context.Background(), "/api/chat", writeSessionTokenValue(1), []byte(`{}`), 200*time.Millisecond, nil)
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
