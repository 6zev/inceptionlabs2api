package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"inceptionlabs2api/internal/clear"
)

// mockClearance is a ClearanceStats that records ProbeNow calls.
type mockClearance struct {
	state    clear.State
	probes   int
	probeErr error
}

func (m *mockClearance) State() clear.State { return m.state }

func (m *mockClearance) ProbeNow(context.Context) error {
	m.probes++
	return m.probeErr
}

// The manual probe exists because a dead ring otherwise recovers only at the
// fill loop's pace (a solve every ~2 s plus a per-slot cooldown), which turned
// a 30-second cookie expiry into minutes of 503s.
func TestHealthzProbeRunsOneAcquisition(t *testing.T) {
	s := newTestServer(t, nil)
	clr := &mockClearance{}
	s.SetClearance(clr)

	req := httptest.NewRequest(http.MethodGet, "/healthz?probe=1", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if clr.probes != 1 {
		t.Fatalf("ProbeNow called %d times, want 1", clr.probes)
	}
}

// A plain /healthz must stay a pure read: an orchestrator polling it every
// second must never trigger proof-of-work solves.
func TestHealthzWithoutProbeIsReadOnly(t *testing.T) {
	s := newTestServer(t, nil)
	clr := &mockClearance{}
	s.SetClearance(clr)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if clr.probes != 0 {
		t.Fatalf("ProbeNow called %d times without ?probe=1", clr.probes)
	}
}

// A failing probe must not turn the health endpoint into a 500: the report is
// what the operator reads to see the failure.
func TestHealthzProbeFailureStillReports(t *testing.T) {
	s := newTestServer(t, nil)
	clr := &mockClearance{probeErr: errMockExhausted}
	s.SetClearance(clr)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz?probe=1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz body is not JSON: %v", err)
	}
	if _, ok := body["clearance"]; !ok {
		t.Fatal("healthz dropped the clearance section after a failed probe")
	}
}
