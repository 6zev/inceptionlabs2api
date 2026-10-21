package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// decideTimeout bounds one test call. It must cover the first clearance
// acquisition (a real PoW solve) and is not the property under test.
const decideTimeout = 30 * time.Second

// addDecideRoutes wires /api/decide/run onto the shared TLS mock.
func (m *mockUpstream) addDecideRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/decide/run", m.handleDecide)
}

func (m *mockUpstream) handleDecide(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.decideTokens = append(m.decideTokens, r.Header.Get("x-session-token"))
	m.decideAccepts = append(m.decideAccepts, r.Header.Get("Accept"))
	body, _ := io.ReadAll(r.Body)
	m.decideBodies = append(m.decideBodies, body)
	h := m.decideHandler
	m.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"answers":{"ok":{"type":"noul","p":0.75}},"inputTokens":12,"latencyMs":3}`)
}

func (m *mockUpstream) decideRequests() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]byte, len(m.decideBodies))
	copy(out, m.decideBodies)
	return out
}

func TestDecideForwardsTheBodyVerbatim(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	c, _ := newTestClient(t, m, 0)

	req := []byte(`{"state":"charged twice","questions":[{"id":"q-1","name":"wants_refund","type":"noul","instructions":"money back?","options":[]}],"images":[]}`)
	got, err := c.Decide(context.Background(), req, decideTimeout)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	want := `{"answers":{"ok":{"type":"noul","p":0.75}},"inputTokens":12,"latencyMs":3}`
	if string(got) != want {
		t.Fatalf("body = %q, want the upstream bytes unchanged", got)
	}
	reqs := m.decideRequests()
	if len(reqs) != 1 {
		t.Fatalf("decide calls = %d", len(reqs))
	}
	if !bytes.Equal(reqs[0], req) {
		t.Fatalf("upstream body = %q, want the client bytes verbatim", reqs[0])
	}
	m.mu.Lock()
	token, accept := m.decideTokens[0], m.decideAccepts[0]
	m.mu.Unlock()
	if token == "" {
		t.Fatal("request carried no x-session-token")
	}
	if accept != "application/json" {
		t.Fatalf("accept = %q", accept)
	}
}

func TestDecideStatusErrorKeepsTheUpstreamSentence(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.decideHandler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"Add at least one question."}`)
	}
	c, _ := newTestClient(t, m, 0)
	_, err := c.Decide(context.Background(), []byte(`{"state":"x","questions":[]}`), decideTimeout)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusBadRequest {
		t.Fatalf("err = %v, want StatusError 400", err)
	}
	if se.Message() != "Add at least one question." {
		t.Fatalf("message = %q", se.Message())
	}
}

func TestDecideRejectsNonJSONBody(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.decideHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<!DOCTYPE html><title>challenge</title>")
	}
	c, _ := newTestClient(t, m, 0)
	_, err := c.Decide(context.Background(), []byte(`{"state":"x"}`), decideTimeout)
	if err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Fatalf("err = %v, want a non-JSON body error", err)
	}
}

func TestDecideRejectsEmptyBody(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.decideHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
	}
	c, _ := newTestClient(t, m, 0)
	_, err := c.Decide(context.Background(), []byte(`{"state":"x"}`), decideTimeout)
	if err == nil {
		t.Fatal("empty body must not be reported as a decision")
	}
}

func TestDecideTimeoutIsIdleTimeout(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	release := make(chan struct{})
	m.decideHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	c, p := newTestClient(t, m, 1)
	t.Cleanup(func() { close(release) })
	// Warm the pool and clearance first: the timeout bounds the call, and the
	// test measures the body phase, not the one-off solve.
	if _, err := p.Get(context.Background()); err != nil {
		t.Fatalf("warm pool: %v", err)
	}
	start := time.Now()
	_, err := c.Decide(context.Background(), []byte(`{"state":"x"}`), 300*time.Millisecond)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("err = %v, want ErrIdleTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Decide waited %s on a stalled body", elapsed)
	}
}

func TestDecideRetriesARejectedToken(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	var attempts atomic.Int32
	m.decideHandler = func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"Unauthorized"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"answers":{},"inputTokens":1,"latencyMs":1}`)
	}
	c, _ := newTestClient(t, m, 2)
	if _, err := c.Decide(context.Background(), []byte(`{"state":"x"}`), decideTimeout); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want a retry on the rejected token", got)
	}
}

func TestDecideResponseIsBounded(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	m.decideHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"answers":{}}`)
		io.WriteString(w, strings.Repeat(" ", maxDecideBytes))
	}
	c, _ := newTestClient(t, m, 0)
	_, err := c.Decide(context.Background(), []byte(`{"state":"x"}`), decideTimeout)
	if err == nil {
		t.Fatal("an oversized body must be an error, not a decision")
	}
}

// The response shape is relayed and not remodelled: the codec must not
// rename p → noul or add a model field on the way out.
func TestDecideDoesNotReshapeAnswers(t *testing.T) {
	t.Parallel()
	m := startUpstream(t)
	raw := `{"answers":{"team":{"type":"choice","choice":"billing","probabilities":{"billing":1},"confidence":1},"urgency":{"type":"score","score":0.98,"levels":[{"label":"Today","p":1}],"confidence":0.9}},"inputTokens":7,"latencyMs":2}`
	m.decideHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, raw)
	}
	c, _ := newTestClient(t, m, 0)
	got, err := c.Decide(context.Background(), []byte(`{"state":"x"}`), decideTimeout)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	var a, b any
	json.Unmarshal([]byte(raw), &a)
	json.Unmarshal(got, &b)
	am, _ := json.Marshal(a)
	bm, _ := json.Marshal(b)
	if !bytes.Equal(am, bm) {
		t.Fatalf("body was reshaped: %s", got)
	}
}
