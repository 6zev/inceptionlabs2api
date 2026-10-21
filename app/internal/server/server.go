// Package server exposes the OpenAI-compatible HTTP surface: chat
// completions (stream and non-stream), models, and health.
package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/pool"
	"inceptionlabs2api/internal/upstream"
)

// maxBodyBytes caps request bodies before JSON decoding.
const maxBodyBytes = 16 << 20

// Chatter is the upstream abstraction the handlers depend on.
type Chatter interface {
	Chat(ctx context.Context, req *upstream.ChatRequest, idle time.Duration, onMalformed func(string)) (*upstream.Stream, error)
}

// PoolStats reports pool size for /healthz.
type PoolStats interface {
	Stats() pool.Stats
}

// Config configures the HTTP surface.
type Config struct {
	APIKey       string
	Model        string
	Models       []string
	ContextChars int
	IdleTimeout  time.Duration
	// Logf is optional; nil means log.Printf.
	Logf func(format string, args ...any)
}

// Counters is a snapshot of request accounting for /healthz.
type Counters struct {
	Requests uint64 `json:"requests"`
	Streams  uint64 `json:"streams"`
	Errors   uint64 `json:"errors"`
	Upstream uint64 `json:"upstream_errors"`
}

// Server holds the handler dependencies.
type Server struct {
	cfg  Config
	up   Chatter
	pool PoolStats
	now  func() time.Time
	logf func(format string, args ...any)

	requests atomic.Uint64
	streams  atomic.Uint64
	errors   atomic.Uint64
	upErrors atomic.Uint64
}

// New builds a Server. Models defaults to [Model].
func New(cfg Config, up Chatter, p PoolStats) *Server {
	if len(cfg.Models) == 0 {
		cfg.Models = []string{cfg.Model}
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	return &Server{cfg: cfg, up: up, pool: p, now: time.Now, logf: logf}
}

// SetClock replaces the clock; tests use it for deterministic created fields.
func (s *Server) SetClock(now func() time.Time) { s.now = now }

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("/v1/models", s.models)
	mux.HandleFunc("/healthz", s.healthz)
	return s.authenticate(cors(mux))
}

// cors answers browser preflights and lets browser-based clients call the
// gateway. The API is keyed, not cookie-based, so a permissive origin is safe
// and avoids a configuration matrix nobody needs.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key")
		h.Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate enforces the optional bearer key. /healthz is always open.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.APIKey == "" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		provided := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			provided = strings.TrimPrefix(h, "Bearer ")
		} else if h := r.Header.Get("X-Api-Key"); h != "" {
			provided = h
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(s.cfg.APIKey)) != 1 {
			openai.WriteError(w, openai.New(http.StatusUnauthorized, "invalid_request_error", openai.CodeInvalidAPIKey, "",
				"invalid API key; send Authorization: Bearer <key>"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		openai.WriteError(w, openai.New(http.StatusMethodNotAllowed, "invalid_request_error", openai.CodeMethodNotAllowed, "",
			"method not allowed; use POST"))
		return
	}
	s.requests.Add(1)

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.fail(w, openai.New(http.StatusRequestEntityTooLarge, "invalid_request_error", openai.CodeRequestTooLarge, "",
				fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes)))
			return
		}
		s.fail(w, openai.New(http.StatusBadRequest, "invalid_request_error", openai.CodeInvalidRequest, "",
			fmt.Sprintf("read request body: %v", err)))
		return
	}

	req, re := openai.Decode(body)
	if re != nil {
		s.fail(w, re)
		return
	}
	if !s.knownModel(req.Model) {
		s.fail(w, openai.New(http.StatusNotFound, "invalid_request_error", openai.CodeModelNotFound, "model",
			fmt.Sprintf("model %q does not exist; available models: %s", req.Model, strings.Join(s.cfg.Models, ", "))))
		return
	}
	msgs, _, re := openai.NormalizeMessages(req.Messages, s.cfg.ContextChars)
	if re != nil {
		s.fail(w, re)
		return
	}

	ureq := upstream.NewChatRequest(msgs, req.ReasoningEffort)
	if req.Stream {
		s.stream(w, r, ureq, req.Model)
		return
	}
	s.complete(w, r, ureq, req.Model)
}

func (s *Server) knownModel(model string) bool {
	for _, m := range s.cfg.Models {
		if m == model {
			return true
		}
	}
	return false
}

// fail records and writes a client-facing error.
func (s *Server) fail(w http.ResponseWriter, re *openai.RequestError) {
	if re.Status >= 500 {
		s.errors.Add(1)
	}
	openai.WriteError(w, re)
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		openai.WriteError(w, openai.New(http.StatusMethodNotAllowed, "invalid_request_error", openai.CodeMethodNotAllowed, "",
			"method not allowed; use GET"))
		return
	}
	created := s.now().Unix()
	list := openai.ModelList{Object: "list", Data: make([]openai.Model, 0, len(s.cfg.Models))}
	for _, m := range s.cfg.Models {
		list.Data = append(list.Data, openai.Model{ID: m, Object: "model", Created: created, OwnedBy: "inceptionlabs"})
	}
	writeJSON(w, http.StatusOK, list)
}

// healthz reports liveness, pool size, and request counters. It is never
// behind auth, so orchestrators can probe it.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		openai.WriteError(w, openai.New(http.StatusMethodNotAllowed, "invalid_request_error", openai.CodeMethodNotAllowed, "",
			"method not allowed; use GET"))
		return
	}
	body := map[string]any{
		"ok": true,
		"counters": Counters{
			Requests: s.requests.Load(),
			Streams:  s.streams.Load(),
			Errors:   s.errors.Load(),
			Upstream: s.upErrors.Load(),
		},
	}
	if s.pool != nil {
		body["pool"] = s.pool.Stats()
	}
	writeJSON(w, http.StatusOK, body)
}
