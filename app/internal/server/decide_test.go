package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"inceptionlabs2api/internal/upstream"
)

// fakeDecider scripts the upstream decision abstraction per call.
type fakeDecider struct {
	mu      sync.Mutex
	bodies  [][]byte
	timeout time.Duration
	fn      func(call int) ([]byte, error)
}

func (f *fakeDecider) Decide(ctx context.Context, body []byte, timeout time.Duration) ([]byte, error) {
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.timeout = timeout
	call := len(f.bodies)
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		return []byte(`{"answers":{"ok":{"type":"noul","p":0.5}},"inputTokens":1,"latencyMs":1}`), nil
	}
	return fn(call)
}

func (f *fakeDecider) lastBody() ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return nil, false
	}
	return f.bodies[len(f.bodies)-1], true
}

func newDecisionServer(d Decider) *Server {
	s := newTestServer(&fakeChatter{})
	s.SetDecider(d)
	return s
}

func postDecision(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

const decisionBody = `{"state":"charged twice","questions":[{"id":"q-1","name":"wants_refund","type":"noul","instructions":"money back?","options":[]}],"images":[]}`

func TestDecisionRelaysTheUpstreamBody(t *testing.T) {
	t.Parallel()
	d := &fakeDecider{}
	s := newDecisionServer(d)
	rec := postDecision(t, s, decisionBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q", cc)
	}
	want := `{"answers":{"ok":{"type":"noul","p":0.5}},"inputTokens":1,"latencyMs":1}`
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("body = %q, want the upstream body verbatim", rec.Body.String())
	}
}

// The web wire is forwarded untouched: questions stays an array; the gateway
// must not convert it to the public API's map or drop images.
func TestDecisionForwardsTheWebShapeVerbatim(t *testing.T) {
	t.Parallel()
	d := &fakeDecider{}
	s := newDecisionServer(d)
	in := `{"state":["x",{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}],"questions":[{"id":"q-1","name":"a","type":"choice","instructions":"i","options":[{"id":"o-1","name":"x","description":"d"}]}],"images":["data:image/png;base64,AA=="],"scenario":"support"}`
	rec := postDecision(t, s, in)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got, ok := d.lastBody()
	if !ok {
		t.Fatal("upstream was not called")
	}
	var want, have any
	json.Unmarshal([]byte(in), &want)
	json.Unmarshal(got, &have)
	wm, _ := json.Marshal(want)
	hm, _ := json.Marshal(have)
	if string(wm) != string(hm) {
		t.Fatalf("upstream body = %s, want the client object", got)
	}
}

func TestDecisionUpstreamErrorUsesTheEnvelope(t *testing.T) {
	t.Parallel()
	d := &fakeDecider{fn: func(int) ([]byte, error) {
		return nil, &upstream.StatusError{Status: http.StatusBadRequest, Body: `{"error":"Add at least one question."}`}
	}}
	s := newDecisionServer(d)
	rec := postDecision(t, s, `{"state":"x","questions":[]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("envelope: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Code != "upstream_error" || env.Error.Type != "upstream_error" {
		t.Fatalf("envelope = %+v", env.Error)
	}
	if env.Error.Message != "Add at least one question." {
		t.Fatalf("message = %q, want the upstream sentence", env.Error.Message)
	}
}

func TestDecisionMethodNotAllowed(t *testing.T) {
	t.Parallel()
	s := newDecisionServer(&fakeDecider{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/decisions", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("allow = %q", allow)
	}
}

func TestDecisionRejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	s := newDecisionServer(&fakeDecider{})
	for _, body := range []string{`{`, `null`, `[]`, `{} {}`} {
		rec := postDecision(t, s, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400", body, rec.Code)
		}
	}
}

func TestDecisionEmptyObjectIsForwarded(t *testing.T) {
	t.Parallel()
	// Under pass-through the validator is the upstream's: an object with no
	// questions is forwarded and the upstream's own sentence comes back.
	d := &fakeDecider{fn: func(int) ([]byte, error) {
		return nil, &upstream.StatusError{Status: http.StatusBadRequest, Body: `{"error":"Add at least one question."}`}
	}}
	s := newDecisionServer(d)
	rec := postDecision(t, s, `{}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want the upstream 400 translated: %s", rec.Code, rec.Body.String())
	}
	if _, ok := d.lastBody(); !ok {
		t.Fatal("the upstream validator was bypassed")
	}
}

func TestDecisionOversizeIs413(t *testing.T) {
	t.Parallel()
	s := newDecisionServer(&fakeDecider{})
	body := `{"state":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	rec := postDecision(t, s, body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestDecisionNoBackendIs503(t *testing.T) {
	t.Parallel()
	s := newTestServer(&fakeChatter{})
	rec := postDecision(t, s, decisionBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestDecisionPassesTheConfiguredTimeout(t *testing.T) {
	t.Parallel()
	d := &fakeDecider{}
	s := newDecisionServer(d)
	s.cfg.DecisionTimeout = 7 * time.Second
	if rec := postDecision(t, s, decisionBody); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	d.mu.Lock()
	got := d.timeout
	d.mu.Unlock()
	if got != 7*time.Second {
		t.Fatalf("timeout = %s", got)
	}
}

func TestDecisionClientGoneWritesNoEnvelope(t *testing.T) {
	t.Parallel()
	d := &fakeDecider{fn: func(int) ([]byte, error) {
		return nil, context.Canceled
	}}
	s := newDecisionServer(d)
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(decisionBody))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("status = %d body = %q, want an untouched recorder", rec.Code, rec.Body.String())
	}
}

func TestDecisionCounter(t *testing.T) {
	t.Parallel()
	s := newDecisionServer(&fakeDecider{})
	postDecision(t, s, decisionBody)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var hz struct {
		Counters struct {
			Decisions uint64 `json:"decisions"`
		} `json:"counters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hz); err != nil {
		t.Fatalf("healthz: %v", err)
	}
	if hz.Counters.Decisions != 1 {
		t.Fatalf("decisions counter = %d", hz.Counters.Decisions)
	}
}

func TestDecisionUpstreamTimeoutIs504(t *testing.T) {
	t.Parallel()
	d := &fakeDecider{fn: func(int) ([]byte, error) {
		return nil, upstream.ErrIdleTimeout
	}}
	s := newDecisionServer(d)
	rec := postDecision(t, s, decisionBody)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rec.Code)
	}
}
