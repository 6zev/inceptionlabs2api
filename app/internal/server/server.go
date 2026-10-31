// Package server exposes the OpenAI-compatible HTTP surface: chat
// completions (stream and non-stream), models, and health.
package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"inceptionlabs2api/internal/clear"
	"inceptionlabs2api/internal/openai"
	"inceptionlabs2api/internal/pool"
	"inceptionlabs2api/internal/tools"
	"inceptionlabs2api/internal/upstream"
)

// maxBodyBytes caps request bodies before JSON decoding. It exists only to
// bound memory use and is far above any request the upstream itself accepts;
// it is not a substitute for the upstream's context limit (SPEC §1.1).
const maxBodyBytes = 6 << 20

// Chatter is the upstream abstraction the handlers depend on.
type Chatter interface {
	Chat(ctx context.Context, req *upstream.ChatRequest, idle time.Duration, onMalformed func(string)) (*upstream.Stream, error)
}

// Searcher is the standalone retrieval abstraction. SearchConfig is called at
// most once per process to expose search.available on /healthz; its failure
// is diagnostic only and never disables the endpoint.
type Searcher interface {
	Search(ctx context.Context, req *upstream.SearchRequest, idle time.Duration) (*upstream.SearchResult, error)
	SearchConfig(ctx context.Context) (*upstream.SearchConfigResponse, error)
}

// PoolStats reports pool size for /healthz.
type PoolStats interface {
	Stats() pool.Stats
}

// ClearanceStats reports checkpoint clearance state for /healthz. A nil
// source means the gateway does not track clearance (e.g. unit tests).
type ClearanceStats interface {
	State() clear.State
	// ProbeNow performs one clearance acquisition (or a no-op when the egress
	// is not challenged). /healthz?probe=1 calls it: an operator who sees a
	// dead ring can re-solve it without restarting the gateway.
	ProbeNow(ctx context.Context) error
}

// Config configures the HTTP surface.
type Config struct {
	APIKey      string
	Model       string
	Models      []string
	IdleTimeout time.Duration
	// MaxMessages caps one chat request's message count after folding, as a
	// last-resort trim. Zero disables it. The upstream rejects >100 messages
	// with "This conversation is too long".
	MaxMessages int
	// EmptyRetries is how many extra upstream attempts an empty completion
	// (no visible text) gets before the gateway reports 502. Zero keeps the
	// single-attempt behaviour.
	EmptyRetries int
	// SearchTimeout bounds /v1/web_search's sliding idle watchdog. Zero means
	// IdleTimeout.
	SearchTimeout time.Duration
	// SearchProbeTimeout bounds the one startup /api/search-lab/config probe.
	// Zero means 15s.
	SearchProbeTimeout time.Duration
	// AudioTimeout bounds the pre-header phase of one /api/tts or /api/stt
	// call. Zero means 120s.
	AudioTimeout time.Duration
	// AudioMaxBytes caps a transcription upload. Zero means 25 MiB.
	AudioMaxBytes int64
	// DecisionTimeout bounds one whole /api/decide/run call. Zero means 120s.
	DecisionTimeout time.Duration
	// PreambleStyle selects the tool preamble skeleton ("hermes" or
	// "inception"). Empty keeps the measured hermes default.
	PreambleStyle string
	// NativeWebSearch auto-declares web_search on tool-bearing chat requests
	// and serves its calls from the attached Searcher. A request that already
	// declares web_search, or a server without a Searcher, keeps the tool off.
	NativeWebSearch bool
	// Logf is optional; nil means log.Printf.
	Logf func(format string, args ...any)
}

// Counters is a snapshot of request accounting for /healthz.
type Counters struct {
	Requests  uint64 `json:"requests"`
	Streams   uint64 `json:"streams"`
	Errors    uint64 `json:"errors"`
	Upstream  uint64 `json:"upstream_errors"`
	TTS       uint64 `json:"tts"`
	STT       uint64 `json:"stt"`
	Decisions uint64 `json:"decisions"`
}

// Server holds the handler dependencies.
type Server struct {
	cfg    Config
	up     Chatter
	se     Searcher
	pool   PoolStats
	clr    ClearanceStats
	audio  Speech
	decide Decider
	now    func() time.Time
	logf   func(format string, args ...any)

	searchMu sync.Mutex
	// searchAvailable is nil until the config probe succeeds; false is a
	// definitive "the deployment disabled retrieval".
	searchAvailable *bool
	// searchProbeErr is the last config-probe failure ("" when it succeeded).
	// Availability alone cannot say "the probe never got through", and on a
	// long-running instance a probe that keeps failing is the first sign of a
	// dead doorway rather than a dead search backend.
	searchProbeErr string
	probeOnce      sync.Once

	requests      atomic.Uint64
	streams       atomic.Uint64
	errors        atomic.Uint64
	upErrors      atomic.Uint64
	tts           atomic.Uint64
	stt           atomic.Uint64
	decisionCalls atomic.Uint64
}

// New builds a Server. Models defaults to [Model].
func New(cfg Config, up Chatter, p PoolStats) *Server {
	if len(cfg.Models) == 0 {
		cfg.Models = []string{cfg.Model}
	}
	// Wire the configured preamble skeleton into the tools package once at
	// boot; empty keeps the measured hermes default.
	if cfg.PreambleStyle != "" {
		tools.PreStyle = cfg.PreambleStyle
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 60 * time.Second
	}
	if cfg.SearchTimeout <= 0 {
		cfg.SearchTimeout = cfg.IdleTimeout
	}
	if cfg.SearchProbeTimeout <= 0 {
		cfg.SearchProbeTimeout = 15 * time.Second
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	return &Server{cfg: cfg, up: up, pool: p, now: time.Now, logf: logf}
}

// SetSearcher attaches the standalone retrieval backend. Without one the
// route answers 503 rather than panicking.
func (s *Server) SetSearcher(se Searcher) { s.se = se }

// SetClearance attaches the clearance state reported by /healthz.
func (s *Server) SetClearance(c ClearanceStats) { s.clr = c }

// StartSearchProbe launches the one-shot /api/search-lab/config probe in the
// background. It is a no-op when no searcher is attached, and repeated calls
// probe only once.
func (s *Server) StartSearchProbe() {
	s.probeOnce.Do(func() {
		if s.se == nil {
			return
		}
		go s.probeSearch(context.Background())
	})
}

// probeSearch records the deployment's retrieval capability. A failed probe
// leaves the value unknown (null) — it never disables the endpoint, because
// the config route can be challenged independently of the run route.
func (s *Server) probeSearch(ctx context.Context) {
	if s.se == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, s.cfg.SearchProbeTimeout)
	defer cancel()
	cfg, err := s.se.SearchConfig(ctx)
	if err != nil {
		s.setSearchProbeError(err.Error())
		s.logf("search: config probe failed (search stays enabled, availability unknown): %v", err)
		return
	}
	if cfg == nil {
		s.logf("search: config probe returned no body; availability unknown")
		return
	}
	available := cfg.RetrievalAvailable
	s.searchMu.Lock()
	s.searchAvailable = &available
	s.searchProbeErr = ""
	s.searchMu.Unlock()
	s.logf("search: retrievalAvailable=%v", available)
}

// setSearchProbeError records a failed probe for /healthz.
func (s *Server) setSearchProbeError(msg string) {
	s.searchMu.Lock()
	s.searchProbeErr = msg
	s.searchMu.Unlock()
}

// searchProbeError returns the last probe failure ("" when none).
func (s *Server) searchProbeError() string {
	s.searchMu.Lock()
	defer s.searchMu.Unlock()
	return s.searchProbeErr
}

// SearchAvailable returns the cached probe result, nil when unknown. The probe
// is advisory: it is reported on /healthz and never gates a request, so a stale
// or wrong value cannot take a working endpoint offline (SPEC §7.1).
func (s *Server) SearchAvailable() *bool {
	s.searchMu.Lock()
	defer s.searchMu.Unlock()
	if s.searchAvailable == nil {
		return nil
	}
	v := *s.searchAvailable
	return &v
}

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("/v1/web_search", s.webSearch)
	mux.HandleFunc("/v1/audio/speech", s.speech)
	mux.HandleFunc("/v1/audio/transcriptions", s.transcription)
	mux.HandleFunc("/v1/decisions", s.decisions)
	mux.HandleFunc("/v1/models", s.models)
	mux.HandleFunc("/healthz", s.healthz)
	return cors(s.authenticate(mux))
}

// cors answers browser preflights and lets browser-based clients call the
// gateway. The API is keyed, not cookie-based, so a permissive origin is safe
// and avoids a configuration matrix nobody needs. It wraps auth because a
// browser preflight never carries credentials: answering 401 to OPTIONS would
// make the documented CORS support unusable whenever a key is set.
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
		methodNotAllowed(w, http.MethodPost)
		return
	}
	s.requests.Add(1)

	body, re := s.readJSONBody(w, r)
	if re != nil {
		s.fail(w, re)
		return
	}
	tag := wireTag()
	wireDumpRequest(tag, body)
	req, re := openai.Decode(body)
	if re != nil {
		s.fail(w, re)
		return
	}
	// Strip the model-switch note at the door, before any path decision:
	// legacy, tools, native — all see the clean user text. The note is the
	// measured sole trigger of the canned identity refusal (il2a-live2 A/B:
	// with it the model refuses outright, identical request minus it answers
	// normally), so stripping here means the refusal never happens and no
	// retry is ever spent. Scope: leading "[Note: ...]" blocks in user-role
	// texts of THIS request only — the client's own stored history is never
	// touched, and the retry-path scrub stays as a belt for other shapes.
	stripSwitchNotes(req)
	if !s.knownModel(req.Model) {
		s.fail(w, openai.New(http.StatusNotFound, "invalid_request_error", openai.CodeModelNotFound, "model",
			fmt.Sprintf("model %q does not exist; available models: %s", req.Model, strings.Join(s.cfg.Models, ", "))))
		return
	}
	// Last-resort message-count cap, applied BEFORE planTools and folding so
	// that (a) tool groups stay atomic, since the trim works on raw messages,
	// and (b) the lossless fold still compresses whatever survives. It only
	// fires above MaxMessages, which sits well past the fold trigger, so in
	// practice the fold alone handles long histories and this is a belt.
	// It runs for every request, tool-bearing or not: a long agent loop is
	// exactly the shape most likely to exceed the upstream cap.
	if trimmed, dropped := trimMessages(req.Messages, s.cfg.MaxMessages); dropped > 0 {
		s.logf("chat: trimmed %d oldest messages (%d -> %d) for the upstream message cap", dropped, len(req.Messages), len(trimmed))
		req.Messages = trimmed
	}

	// Tool-bearing requests take §4.1's path, which normalises history with
	// tool payloads intact. A nil plan means the legacy one, and it must stay
	// byte-identical for every request without tools.
	plan, re := planTools(req, s.nativeSearchTool())
	if re != nil {
		s.fail(w, re)
		return
	}
	if plan != nil {
		s.chatWithTools(w, r, req, plan)
		return
	}

	msgs, _, re := openai.NormalizeMessages(req.Messages)
	if re != nil {
		s.fail(w, re)
		return
	}

	// Fold long histories past the upstream limit before conversion. The
	// plain path has no preamble message, so the fold trigger is the history
	// length alone; the teaching section rides inside the bubble. Below the
	// trigger the messages cross untouched — byte-identical to no-fold.
	if len(msgs) > openai.FoldTrigger {
		hist := make([]openai.History, 0, len(msgs))
		for _, m := range msgs {
			hist = append(hist, openai.History{Role: m.Role, Texts: m.Texts})
		}
		if folded, did := openai.FoldHistory(hist, false); did {
			msgs = make([]openai.TextMessage, 0, len(folded))
			for _, h := range folded {
				msgs = append(msgs, openai.TextMessage{Role: h.Role, Texts: h.Texts})
			}
		}
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
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	created := s.now().Unix()
	list := openai.ModelList{Object: "list", Data: make([]openai.Model, 0, len(s.cfg.Models))}
	for _, m := range s.cfg.Models {
		list.Data = append(list.Data, s.modelEntry(m, created))
	}
	writeJSON(w, http.StatusOK, list)
}

// modelEntry builds one /v1/models row from what the gateway can actually
// vouch for. This is the only place a client learns that thinking exists at
// all: without a capability signal, a UI never offers a level selector and
// never sends reasoning_effort, so the reasoning_content the chat handlers
// already forward goes unread.
//
// The values are configuration and published model data, never fabrication.
// Context and output limits come from the model card (see
// upstream.MaxContextTokens); the message and byte caps this deployment really
// enforces are reported in limits.
func (s *Server) modelEntry(id string, created int64) openai.Model {
	limits := openai.ModelLimits{
		// MaxMessages already reserves headroom under the upstream's hard 100
		// (default 96). Zero disables the trim, which means the upstream's own
		// 100 applies, so that is what is advertised.
		MaxMessages:   s.cfg.MaxMessages,
		MaxInputBytes: upstream.MaxInputBytes,
	}
	if limits.MaxMessages <= 0 {
		limits.MaxMessages = upstream.MaxUpstreamMessages
	}
	return openai.Model{
		ID:      id,
		Object:  "model",
		Created: created,
		OwnedBy: "inceptionlabs",
		Capabilities: openai.ModelCapabilities{
			Streaming: true,
			Tools:     true,
			// Reasoning exists at effort >= low, which is the gateway default
			// (medium). See ReasoningLevels for the caveat on instant.
			Reasoning: true,
			WebSearch: s.cfg.NativeWebSearch && s.se != nil,
		},
		SupportsReasoning: true,
		ReasoningLevels:   []string{"instant", "low", "medium", "high"},
		Limits:            limits,

		ContextLength: upstream.MaxContextTokens,
		Architecture: openai.ModelArchitecture{
			Modality:         "text->text",
			InputModalities:  []string{"text"},
			OutputModalities: []string{"text"},
			Tokenizer:        "Other",
		},
		// Only what the gateway parses and forwards: unknown request fields are
		// dropped (README), so listing temperature or max_tokens here would
		// promise settings that never reach the backend.
		SupportedParameters: []string{
			"tools",
			"tool_choice",
			"parallel_tool_calls",
			"reasoning",
			"reasoning_effort",
			"stream",
		},
		Pricing: openai.ModelPricing{
			Prompt:            "0",
			Completion:        "0",
			Request:           "0",
			Image:             "0",
			WebSearch:         "0",
			InternalReasoning: "0",
		},
		TopProvider: openai.ModelTopProvider{
			ContextLength:       upstream.MaxContextTokens,
			MaxCompletionTokens: upstream.MaxOutputTokens,
			IsModerated:         false,
		},
	}
}

// healthz reports liveness, pool size, and request counters. It is never
// behind auth, so orchestrators can probe it.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	// ?probe=1 forces one clearance acquisition before reporting. It exists
	// because the only other recovery from a dead ring was a restart: the
	// background loop paces one solve every ~2 s and a failing slot waits out
	// its cooldown, so an operator watching a 3-4 minute outage had no lever.
	// Bounded, and never gated behind auth failure: /healthz is already open.
	if r.URL.Query().Get("probe") == "1" && s.clr != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		err := s.clr.ProbeNow(ctx)
		cancel()
		if err != nil {
			s.logf("clear: manual probe failed: %v", err)
		}
	}
	body := map[string]any{
		"ok": true,
		"counters": Counters{
			Requests:  s.requests.Load(),
			Streams:   s.streams.Load(),
			Errors:    s.errors.Load(),
			Upstream:  s.upErrors.Load(),
			TTS:       s.tts.Load(),
			STT:       s.stt.Load(),
			Decisions: s.decisionCalls.Load(),
		},
		"search": map[string]any{"available": s.SearchAvailable(), "probe": s.searchProbeError()},
	}
	if s.pool != nil {
		body["pool"] = s.pool.Stats()
	}
	if s.clr != nil {
		body["clearance"] = s.clr.State()
	}
	writeJSON(w, http.StatusOK, body)
}
