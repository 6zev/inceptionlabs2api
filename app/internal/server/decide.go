package server

import (
	"context"
	"net/http"
	"time"

	"inceptionlabs2api/internal/openai"
)

// Decider is the upstream decision abstraction the handler depends on. It is
// separate from Chatter and Speech so a deployment can wire it independently.
type Decider interface {
	Decide(ctx context.Context, body []byte, timeout time.Duration) ([]byte, error)
}

// defaultDecisionTimeout bounds one whole /api/decide/run call. The live
// route answered in ~0.2 s; the window is generous for a large state and
// eight questions.
const defaultDecisionTimeout = 120 * time.Second

// errNoDecisionBackend is what the route answers when no Decider is attached;
// it is a configuration fault, not an upstream one.
var errNoDecisionBackend = openai.New(http.StatusServiceUnavailable, "server_error", openai.CodeUpstreamUnavailable, "",
	"decision backend is not configured")

// SetDecider attaches the decision backend. Without one the route answers 503
// rather than panicking.
func (s *Server) SetDecider(d Decider) { s.decide = d }

// decisionTimeout returns the configured timeout or its default.
func (s *Server) decisionTimeout() time.Duration {
	if s.cfg.DecisionTimeout > 0 {
		return s.cfg.DecisionTimeout
	}
	return defaultDecisionTimeout
}

// decisions relays POST /api/decide/run (SPEC §2.6, §7.4). The request and
// the response are the upstream web shapes as-is: the body is forwarded
// verbatim and the answer is relayed byte for byte. Only auth and failures
// are translated. The route is non-streaming because the upstream is.
func (s *Server) decisions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	s.requests.Add(1)
	s.decisionCalls.Add(1)

	body, re := s.readJSONBody(w, r)
	if re != nil {
		s.fail(w, re)
		return
	}
	// Structural check only: one JSON object, nothing after it. What the
	// object contains is the upstream validator's business (§1.1).
	if _, re := decodeJSONObject(body); re != nil {
		s.fail(w, re)
		return
	}
	if s.decide == nil {
		s.fail(w, errNoDecisionBackend)
		return
	}

	out, err := s.decide.Decide(r.Context(), body, s.decisionTimeout())
	if err != nil {
		if clientGone(r, err) {
			return
		}
		s.fail(w, s.upstreamError(err))
		return
	}
	// The answer derives from the caller's state; keep it out of shared
	// caches even though the upstream sends a cacheable page default.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
