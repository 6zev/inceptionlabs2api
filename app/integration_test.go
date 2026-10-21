package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"inceptionlabs2api/internal/pool"
	"inceptionlabs2api/internal/server"
	"inceptionlabs2api/internal/upstream"
)

// stack is the full gateway wired to a TLS mock upstream.
type stack struct {
	srv    *httptest.Server
	pool   *pool.Pool
	client *upstream.Client
	mock   *tlsUpstream
}

type tlsUpstream struct {
	ln     net.Listener
	roots  *x509.CertPool
	base   string
	mu     sync.Mutex
	tokens []string
	chat   func(w http.ResponseWriter, r *http.Request)
	status atomic.Int32
}

func (m *tlsUpstream) setChat(fn func(http.ResponseWriter, *http.Request)) {
	m.mu.Lock()
	m.chat = fn
	m.mu.Unlock()
}

func newTLSUpstream(t *testing.T) *tlsUpstream {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
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
		t.Fatalf("cert: %v", err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("keypair: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	m := &tlsUpstream{ln: ln, roots: roots, base: "https://" + ln.Addr().String()}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		if m.status.Load() != 0 {
			w.WriteHeader(int(m.status.Load()))
			io.WriteString(w, "<html>Vercel Security Checkpoint</html>")
			return
		}
		var n int
		m.mu.Lock()
		n = len(m.tokens) + 1
		m.mu.Unlock()
		nonce := strings.Repeat("0", 31) + string(rune('a'+n%26))
		token := "1792553057." + nonce + "." + strings.Repeat("f", 64)
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "token": token})
	})
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.tokens = append(m.tokens, r.Header.Get("x-session-token"))
		fn := m.chat
		m.mu.Unlock()
		if fn != nil {
			fn(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return m
}

func (m *tlsUpstream) tokenCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.tokens)
}

func newStack(t *testing.T, idle time.Duration) *stack {
	t.Helper()
	m := newTLSUpstream(t)
	c, fetch := upstream.NewClient(upstream.Config{
		BaseURL:        m.base,
		SessionTimeout: 5 * time.Second,
		RootCAs:        m.roots,
		Logf:           func(string, ...any) {},
	})
	p := pool.New(pool.Config{Min: 2, Max: 8, TTL: 30 * time.Minute, Interval: time.Hour}, fetch)
	t.Cleanup(p.Close)
	c.BindPool(p)

	h := server.New(server.Config{
		Model:        "mercury-3",
		ContextChars: 250_000,
		IdleTimeout:  idle,
		Logf:         func(string, ...any) {},
	}, c, p).Handler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &stack{srv: srv, pool: p, client: c, mock: m}
}

const realisticStream = `data: {"type":"reasoning-start","id":"reasoning-0"}

data: {"type":"reasoning-delta","id":"reasoning-0","delta":"Analyzing prime congruences.\n"}

data: {"type":"reasoning-delta","id":"reasoning-0","delta":"Searching for recent proofs.\n"}

data: {"type":"reasoning-end","id":"reasoning-0"}

data: {"type":"text-start","id":"text-0"}

data: {"type":"text-delta","id":"text-0","delta":"Twin primes are conjectured "}

data: {"type":"text-delta","id":"text-0","delta":"to be infinite."}

data: {"type":"text-end","id":"text-0"}

data: [DONE]

`

func chatBody(stream bool) string {
	if stream {
		return `{"model":"mercury-3","stream":true,"messages":[{"role":"user","content":"prove it"}]}`
	}
	return `{"model":"mercury-3","messages":[{"role":"user","content":"prove it"}]}`
}

func TestEndToEndStream(t *testing.T) {
	t.Parallel()
	st := newStack(t, 5*time.Second)
	st.mock.setChat(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, realisticStream)
	})

	resp, err := http.Post(st.srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody(true)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var reasoning, content strings.Builder
	var finish string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Reasoning string `json:"reasoning_content"`
					Content   string `json:"content"`
				} `json:"delta"`
				Finish *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk %q: %v", payload, err)
		}
		reasoning.WriteString(chunk.Choices[0].Delta.Reasoning)
		content.WriteString(chunk.Choices[0].Delta.Content)
		if chunk.Choices[0].Finish != nil {
			finish = *chunk.Choices[0].Finish
		}
	}
	if reasoning.String() != "Analyzing prime congruences.\nSearching for recent proofs.\n" {
		t.Fatalf("reasoning = %q", reasoning.String())
	}
	if content.String() != "Twin primes are conjectured to be infinite." {
		t.Fatalf("content = %q", content.String())
	}
	if finish != "stop" {
		t.Fatalf("finish = %q", finish)
	}
}

func TestEndToEndNonStream(t *testing.T) {
	t.Parallel()
	st := newStack(t, 5*time.Second)
	st.mock.setChat(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, realisticStream)
	})
	resp, err := http.Post(st.srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody(false)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), `"usage"`) {
		t.Fatalf("usage must be omitted: %s", raw)
	}
	var body struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Object != "chat.completion" {
		t.Fatalf("object = %q", body.Object)
	}
	if body.Choices[0].Message.Content != "Twin primes are conjectured to be infinite." {
		t.Fatalf("content = %q", body.Choices[0].Message.Content)
	}
	if body.Choices[0].Message.Reasoning == "" {
		t.Fatal("reasoning_content missing")
	}
}

func TestEndToEndClientDisconnectKeepsPool(t *testing.T) {
	t.Parallel()
	st := newStack(t, 30*time.Second)
	release := make(chan struct{})
	defer close(release)
	st.mock.setChat(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"text-delta\",\"id\":\"text-0\",\"delta\":\"first\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, st.srv.URL+"/v1/chat/completions", strings.NewReader(chatBody(true)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	reader := bufio.NewReader(resp.Body)
	// Read the role frame and wait for the first content frame, then abort.
	for i := 0; i < 3; i++ {
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatalf("read frame: %v", err)
		}
	}
	cancel()
	resp.Body.Close()

	// Let the server observe the cancellation.
	time.Sleep(100 * time.Millisecond)
	if got := st.pool.Stats().Size; got != 2 {
		t.Fatalf("pool size = %d after a client disconnect, want 2 (tokens must not be poisoned)", got)
	}
	if got := st.mock.tokenCount(); got != 1 {
		t.Fatalf("upstream chat attempts = %d, want 1", got)
	}

	// The pool still serves: a second request must not need a new token.
	st.mock.setChat(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"text-delta\",\"id\":\"text-0\",\"delta\":\"ok\"}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	resp2, err := http.Post(st.srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody(true)))
	if err != nil {
		t.Fatalf("second POST: %v", err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if got := st.pool.Stats().Size; got != 2 {
		t.Fatalf("pool size = %d after two requests", got)
	}
}

func TestEndToEndMissingSessionIs503(t *testing.T) {
	t.Parallel()
	st := newStack(t, 5*time.Second)
	// Drain the pool: both tokens are rejected, then sessions start failing.
	st.mock.status.Store(http.StatusTooManyRequests)
	for _, tok := range []string{"a", "b"} {
		st.pool.Discard("1792553057." + strings.Repeat("0", 31) + tok + "." + strings.Repeat("f", 64))
	}
	deadline := time.Now().Add(2 * time.Second)
	for st.pool.Stats().Size > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := http.Post(st.srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody(true)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body %s, want 503", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "upstream_unavailable") {
		t.Fatalf("body = %s", raw)
	}
}

func TestEndToEndUpstreamIdle(t *testing.T) {
	t.Parallel()
	st := newStack(t, 120*time.Millisecond)
	block := make(chan struct{})
	defer close(block)
	st.mock.setChat(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"text-delta\",\"id\":\"text-0\",\"delta\":\"a\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-block:
		case <-r.Context().Done():
		}
	})
	resp, err := http.Post(st.srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody(true)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "upstream_idle_timeout") {
		t.Fatalf("missing idle error: %s", raw)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(raw)), "[DONE]") {
		t.Fatalf("missing [DONE]: %s", raw)
	}
	if got := st.pool.Stats().Size; got != 2 {
		t.Fatalf("pool size = %d after idle timeout, want 2", got)
	}
}

func TestEndToEndNon200(t *testing.T) {
	t.Parallel()
	st := newStack(t, 5*time.Second)
	st.mock.setChat(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "<html>challenge secret</html>")
	})
	resp, err := http.Post(st.srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody(true)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("upstream body leaked: %s", raw)
	}
}

func TestEndToEndAuthAndModels(t *testing.T) {
	t.Parallel()
	m := newTLSUpstream(t)
	c, fetch := upstream.NewClient(upstream.Config{BaseURL: m.base, RootCAs: m.roots, Logf: func(string, ...any) {}})
	p := pool.New(pool.Config{Min: 0, Max: 2}, fetch)
	defer p.Close()
	c.BindPool(p)
	h := server.New(server.Config{APIKey: "k", Model: "mercury-3", Logf: func(string, ...any) {}}, c, p).Handler()
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, _ := http.Get(srv.URL + "/v1/models")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("models without key: %d", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer k")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("models: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models with key: %d", resp.StatusCode)
	}
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Data) != 1 || list.Data[0].ID != "mercury-3" {
		t.Fatalf("models = %+v", list)
	}

	health, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %d", health.StatusCode)
	}
}

// TestMainEntrypoint boots the real process wiring on a random port, probes
// /healthz, and shuts down through SIGTERM.
func TestMainEntrypoint(t *testing.T) {
	m := newTLSUpstream(t)
	t.Setenv("IL2API_ADDR", "127.0.0.1:0")
	t.Setenv("IL2API_UPSTREAM_BASE", m.base)
	t.Setenv("IL2API_MODEL", "mercury-3")
	t.Setenv("IL2API_POOL_MIN", "1")
	t.Setenv("IL2API_POOL_MAX", "1")
	t.Setenv("IL2API_API_KEY", "key")

	// run() blocks until signalled; report startup errors instead of exiting.
	errCh := make(chan error, 1)
	go func() { errCh <- run() }()
	time.Sleep(150 * time.Millisecond)
	proc, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("find process: %v", err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("run returned %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not shut down")
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	t.Setenv("IL2API_POOL_MIN", "99")
	t.Setenv("IL2API_POOL_MAX", "8")
	if err := run(); err == nil || !strings.Contains(err.Error(), "POOL_MIN") {
		t.Fatalf("run = %v, want pool config error", err)
	}
}
