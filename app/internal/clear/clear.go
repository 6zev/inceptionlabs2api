// Package clear holds the Vercel Security Checkpoint clearance: it solves the
// v2 proof-of-work inline and replays the resulting _vcrcs cookie. It also
// builds the one stdlib HTTP transport every upstream request uses.
//
// The design is the one verified live in /root/vercel-bypass/REPORT.md:
// clearance requires the _vcrcs cookie + the Chrome UA that solved + HTTP/2.
// It is NOT bound to a TLS fingerprint, so forcing a utls ClientHello with
// ALPN http/1.1 (the previous design) could never pass.
package clear

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// UserAgent is the single Chrome UA used for clearance acquisition and for
// every upstream request. The cookie is UA-bound, so the two must never
// diverge; callers must not set their own User-Agent.
const UserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// challengePath is the checkpoint's solution-submission endpoint.
const challengePath = "/.well-known/vercel/security/request-challenge"

// CookieName is the clearance cookie the edge sets.
const CookieName = "_vcrcs"

// Defaults for the clearance lifecycle.
const (
	// DefaultRefresh is the age at which a cookie is proactively replaced:
	// 45 minutes into the edge's 60-minute TTL.
	DefaultRefresh = 45 * time.Minute
	// DefaultCooldown is the minimum spacing between acquisitions. A burst of
	// challenged requests must not turn into a solve storm.
	DefaultCooldown = 15 * time.Second
	// DefaultTimeout bounds one acquisition (GET + POST).
	DefaultTimeout = 30 * time.Second
	// DefaultPool is the ring size. Live probes (§2.1.2) showed cookies are
	// fungible with no per-cookie penalty; 3 hedges a per-slot retirement
	// without paying more than three solves at boot.
	DefaultPool = 3
	// maxBodyBytes drops what we read from challenge responses; only headers
	// and cookies matter.
	maxBodyBytes = 1 << 20
)

// ErrRefused means the edge answered the submission with 403: the proof of
// work itself was rejected (checkpoint version drift). Retrying cannot help.
var ErrRefused = errors.New("clear: challenge solution refused")

// errNoChallenge means the edge answered the probe without a challenge, so
// there is nothing to clear: this egress is already welcome.
var errNoChallenge = errors.New("clear: no challenge issued")

// State is the /healthz view of clearance. The top-level fields describe the
// ring as a whole (HasCookie = any live slot, AgeSeconds = the oldest live
// slot); Slots reports each configured slot.
type State struct {
	HasCookie    bool        `json:"has_cookie"`
	AgeSeconds   int64       `json:"age_seconds"`
	RefreshCount int64       `json:"refresh_count"`
	Slots        []SlotState `json:"slots"`
}

// SlotState is the health view of one ring slot.
type SlotState struct {
	Cookie       bool  `json:"cookie"`
	AgeSeconds   int64 `json:"age_seconds"`
	RefreshCount int64 `json:"refresh_count"`
}

// slot is one clearance cookie in the rotation ring.
type slot struct {
	cookie    string
	acquired  time.Time
	refreshes int64
}

// Config configures a Clearance.
type Config struct {
	// BaseURL is the upstream origin, e.g. https://chat.inceptionlabs.ai.
	BaseURL string
	// Client is the HTTP client acquisition uses. It must share the egress
	// (proxy) the gateway's other requests use: the cookie is IP-bound.
	Client *http.Client
	// Pool is the number of cookies held in the rotation ring. Zero means
	// DefaultPool; 1 is the single-cookie behaviour.
	Pool int
	// Refresh is the proactive replacement age. Zero means DefaultRefresh.
	Refresh time.Duration
	// Cooldown is the minimum spacing between acquisitions. Zero means
	// DefaultCooldown; negative disables it (tests).
	Cooldown time.Duration
	// Timeout bounds one acquisition. Zero means DefaultTimeout.
	Timeout time.Duration
	// Now is injectable for tests. Nil means time.Now.
	Now func() time.Time
	// Logf is optional; nil discards.
	Logf func(format string, args ...any)
}

// Clearance holds a ring of cookies. It is safe for concurrent use; refresh
// work is single-flight across the whole ring.
type Clearance struct {
	cfg    Config
	base   string
	client *http.Client
	now    func() time.Time
	logf   func(format string, args ...any)

	mu sync.Mutex
	// slots is fixed-length (Config.Pool); an empty cookie is a free slot.
	slots []slot
	// cursor is the round-robin position; Header advances it by one request.
	cursor uint64
	// probed records a successful "this egress is not challenged" result.
	// Without it Ensure would perform the /api/session probe before every
	// upstream request, doubling the session traffic the pool already makes.
	probed bool
	// probedAt is when that result was recorded; it is re-checked on the same
	// schedule a cookie would be refreshed.
	probedAt time.Time

	refreshes   int64
	lastAttempt time.Time
	// lastFailed records that the most recent attempt failed, so a cooldown
	// window opened by a success is not mistaken for a failing checkpoint.
	lastFailed bool
	lastErr    error

	inflight     bool
	inflightDone chan struct{}
}

// New builds a Clearance. A nil Client means a client over a default stdlib
// transport, which is enough for tests but not for a proxy setup.
func New(cfg Config) *Clearance {
	if cfg.Pool <= 0 {
		cfg.Pool = DefaultPool
	}
	if cfg.Refresh <= 0 {
		cfg.Refresh = DefaultRefresh
	}
	if cfg.Cooldown == 0 {
		cfg.Cooldown = DefaultCooldown
	}
	if cfg.Cooldown < 0 {
		cfg.Cooldown = 0
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Transport: Transport(TransportOptions{})}
	}
	return &Clearance{
		cfg:    cfg,
		base:   strings.TrimRight(cfg.BaseURL, "/"),
		client: client,
		now:    cfg.Now,
		logf:   logf,
		slots:  make([]slot, cfg.Pool),
	}
}

// Header returns the UA and the Cookie header value to send, rotating the ring
// by one slot per call. The cookie is empty when no slot is live.
//
// Rotation is per request, not per token, retry or response (§2.1.2). A slot
// past the refresh margin is skipped while a live one exists; when every slot
// is stale the ring still serves round-robin — a request never waits on a
// solve that the refiller is already going to make.
func (c *Clearance) Header() (ua, cookie string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	n := uint64(len(c.slots))

	// Prefer the next live, fresh slot at or after the cursor.
	for i := uint64(0); i < n; i++ {
		idx := (c.cursor + i) % n
		if s := c.slots[idx]; s.cookie != "" && now.Sub(s.acquired) < c.cfg.Refresh {
			c.cursor = idx + 1
			return UserAgent, CookieName + "=" + s.cookie
		}
	}
	// No fresh slot: take the next live one, stale or not.
	for i := uint64(0); i < n; i++ {
		idx := (c.cursor + i) % n
		if c.slots[idx].cookie != "" {
			c.cursor = idx + 1
			return UserAgent, CookieName + "=" + c.slots[idx].cookie
		}
	}
	return UserAgent, ""
}

// Cookie returns the value of the next live slot ("" when none). It does not
// advance the cursor; it exists for tests and diagnostics.
func (c *Clearance) Cookie() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.slots {
		if s.cookie != "" {
			return s.cookie
		}
	}
	return ""
}

// State reports clearance health for /healthz.
func (c *Clearance) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	st := State{RefreshCount: c.refreshes, Slots: make([]SlotState, len(c.slots))}
	oldest := time.Duration(-1)
	for i, s := range c.slots {
		ss := SlotState{Cookie: s.cookie != "", RefreshCount: s.refreshes}
		if s.cookie != "" {
			age := now.Sub(s.acquired)
			if age < 0 {
				age = 0
			}
			ss.AgeSeconds = int64(age.Seconds())
			if oldest < 0 || age > oldest {
				oldest = age
				st.AgeSeconds = ss.AgeSeconds
			}
			st.HasCookie = true
		}
		st.Slots[i] = ss
	}
	return st
}

// staleLocked reports whether a request-path acquisition is needed: no live
// cookie at all, or every live slot past the refresh margin.
func (c *Clearance) staleLocked() bool {
	if c.liveLocked() == 0 {
		// An unchallenged egress holds no cookie but is still fresh for one
		// refresh window, for the same reason a cookie is.
		if !c.probed {
			return true
		}
		return c.now().Sub(c.probedAt) >= c.cfg.Refresh
	}
	_, stale := c.staleSlotLocked()
	// Only when *every* slot is stale: a partially aged ring is the refiller's
	// job, and Header still serves the stale slot rather than blocking.
	fresh := 0
	for _, s := range c.slots {
		if s.cookie != "" && c.now().Sub(s.acquired) < c.cfg.Refresh {
			fresh++
		}
	}
	return stale && fresh == 0
}

// liveLocked counts slots holding a cookie.
func (c *Clearance) liveLocked() int {
	n := 0
	for _, s := range c.slots {
		if s.cookie != "" {
			n++
		}
	}
	return n
}

// Ensure acquires a cookie if none is held, or replaces a slot past the refresh
// margin when every slot is stale. Concurrent callers coalesce into one
// acquisition. The per-request path never tops the ring up: rotation plus the
// boot fill and RefreshStale are what maintain its size.
func (c *Clearance) Ensure(ctx context.Context) error {
	c.mu.Lock()
	needed := c.staleLocked()
	c.mu.Unlock()
	if !needed {
		return nil
	}
	// An explicit replacement (a stale ring) is not the lazy success-cooldown
	// shortcut; only a failure window may stop it.
	c.mu.Lock()
	idx := c.emptyLocked()
	c.mu.Unlock()
	return c.acquireForce(ctx, idx)
}

// Refresh replaces the cookie a challenge just rejected, or acquires one when
// none is held. It is the recovery path: a window opened by a failure still
// spaces it, but it always attempts a solve when no healthy replay is
// possible, even inside a success cooldown.
func (c *Clearance) Refresh(ctx context.Context) error {
	c.mu.Lock()
	idx := c.emptyLocked()
	c.mu.Unlock()
	// Always force: a lazy success-cooldown skip would tell the caller "done"
	// while the cookie it asked to replace still stands.
	return c.acquireForce(ctx, idx)
}

// Prewarm starts the boot fill in the background and returns immediately. Boot
// calls it so the first request does not pay the solve and network round trip
// (SPEC §2.1.1); a failure is logged and left to the ordinary lazy path
// (Ensure on the next request, Refresh on a challenge). The goroutine is
// bounded by the acquisition timeout each acquisition inherits.
func (c *Clearance) Prewarm(ctx context.Context) {
	go func() {
		if err := c.PrewarmNow(ctx); err != nil {
			c.logf("clear: prewarm failed (requests will retry lazily): %v", err)
		}
	}()
}

// PrewarmNow fills every slot synchronously and reports the first failure.
// Acquisitions are sequential (§2.1.2): concurrent solves are the storm the
// cooldown exists to prevent. A failed fill stops there; the slots already
// filled stay live, and Refill/RefreshStale recover the rest.
func (c *Clearance) PrewarmNow(ctx context.Context) error {
	c.mu.Lock()
	empty := c.liveLocked() < c.cfg.Pool
	c.mu.Unlock()
	if !empty {
		return nil
	}
	if err := c.Ensure(ctx); err != nil {
		return err
	}
	for {
		c.mu.Lock()
		need := c.liveLocked() < c.cfg.Pool && !c.probed
		c.mu.Unlock()
		if !need {
			return nil
		}
		if err := c.acquireForce(ctx, c.emptyLocked()); err != nil {
			return err
		}
	}
}

// RunRefreshLoop keeps the ring fresh from boot, checking at every tick for
// slots at the configured refresh age and replacing at most one per tick, so a
// multi-slot ring does not burst its solves (§2.1.2).
//
// It only maintains cookies it already holds: when none exists (an egress that
// is never challenged, or a failed prewarm) it does nothing, because Ensure is
// the path that retries acquisition and prewarm already ran once.
//
// It blocks until ctx is cancelled. Callers run it in a goroutine next to
// Prewarm.
func (c *Clearance) RunRefreshLoop(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.Cookie() == "" {
				continue
			}
			if err := c.RefreshStale(ctx); err != nil {
				c.logf("clear: proactive refresh failed: %v", err)
			}
		}
	}
}

// RefreshStale replaces at most one slot that has reached the refresh age.
func (c *Clearance) RefreshStale(ctx context.Context) error {
	c.mu.Lock()
	idx, have := c.staleSlotLocked()
	c.mu.Unlock()
	if !have {
		return nil
	}
	return c.acquireForce(ctx, idx)
}

// staleSlotLocked returns the oldest slot past the refresh margin.
func (c *Clearance) staleSlotLocked() (int, bool) {
	now := c.now()
	idx, oldest := -1, time.Duration(0)
	for i, s := range c.slots {
		if s.cookie == "" {
			continue
		}
		age := now.Sub(s.acquired)
		if age < c.cfg.Refresh {
			continue
		}
		if idx < 0 || age > oldest {
			idx, oldest = i, age
		}
	}
	return idx, idx >= 0
}

// Retire removes the slot holding cookie (a value with or without the
// "_vcrcs=" prefix). A 429 proves that cookie is not accepted; only that slot
// is retired, and it is refilled in the background (SPEC §2.1.2). An unknown
// value is a no-op.
func (c *Clearance) Retire(cookie string) error {
	cookie = strings.TrimPrefix(cookie, CookieName+"=")
	c.mu.Lock()
	for i := range c.slots {
		if c.slots[i].cookie == cookie {
			c.slots[i] = slot{}
			c.mu.Unlock()
			c.logf("clear: retired slot %d after a challenge", i)
			return nil
		}
	}
	c.mu.Unlock()
	return nil
}

// Invalidate forgets every cookie so the next Ensure acquires fresh ones.
func (c *Clearance) Invalidate() {
	c.mu.Lock()
	c.slots = make([]slot, c.cfg.Pool)
	c.probed = false
	c.probedAt = time.Time{}
	c.cursor = 0
	c.mu.Unlock()
}

// acquireForce runs one single-flight acquisition into a chosen slot. It is
// the only entry below "replace something": every caller (a challenge replay,
// a stale slot, a retired slot, the boot fill) is an explicit instruction, and
// the only thing that may stop it is a recorded *failure* inside the
// cooldown. idx < 0 means "choose a slot".
func (c *Clearance) acquireForce(ctx context.Context, idx int) error {
	return c.acquireSlot(ctx, idx)
}

// Refill acquires one cookie into an empty slot, or replaces the oldest slot
// when the ring is full. It is what a retired slot's background refill calls.
func (c *Clearance) Refill(ctx context.Context) error {
	c.mu.Lock()
	idx := c.emptyLocked()
	c.mu.Unlock()
	return c.acquireSlot(ctx, idx)
}

// emptyLocked returns the index of the first empty slot, or the index of the
// oldest slot when the ring is full.
func (c *Clearance) emptyLocked() int {
	oldestIdx, oldest := 0, time.Duration(-1)
	for i, s := range c.slots {
		if s.cookie == "" {
			return i
		}
		age := c.now().Sub(s.acquired)
		if oldest < 0 || age > oldest {
			oldestIdx, oldest = i, age
		}
	}
	return oldestIdx
}

// acquireSlot runs at most one acquisition at a time, writing the result into
// slot idx.
func (c *Clearance) acquireSlot(ctx context.Context, idx int) error {
	c.mu.Lock()
	if c.inflight {
		done := c.inflightDone
		c.mu.Unlock()
		return c.wait(ctx, done)
	}
	c.inflight = true
	c.inflightDone = make(chan struct{})
	done := c.inflightDone
	c.mu.Unlock()

	err := c.refresh(ctx, idx)

	c.mu.Lock()
	c.inflight = false
	c.inflightDone = nil
	if err != nil {
		c.lastErr = err
		c.lastFailed = true
	} else {
		c.lastErr = nil
		c.lastFailed = false
	}
	c.mu.Unlock()
	close(done)
	return err
}

// wait blocks until the owner's acquisition finishes and reports its result.
func (c *Clearance) wait(ctx context.Context, done chan struct{}) error {
	select {
	case <-done:
		c.mu.Lock()
		defer c.mu.Unlock()
		// A successful acquisition may legitimately end with no cookie: an
		// unchallenged egress needs none. Only a recorded error is a failure.
		return c.lastErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// refresh runs the GET -> solve -> POST flow, honouring the cooldown.
//
// The cooldown bounds *failed* attempts: a burst of requests against a
// checkpoint that keeps refusing must not become a solve storm (SPEC §2.1.1).
// Inside a window opened by a failure, a held cookie is reused (the caller's
// request proceeds with it) and a caller without one gets lastErr back rather
// than a silent success that would send an unprotected request. A window never
// follows a successful acquisition, so recovery — a 429 challenge right after
// a solve — is never blocked.
func (c *Clearance) refresh(ctx context.Context, idx int) error {
	now := c.now()
	c.mu.Lock()
	last := c.lastAttempt
	lastErr := c.lastErr
	openedByFailure := c.lastFailed
	c.mu.Unlock()

	if c.cfg.Cooldown > 0 && !last.IsZero() && now.Sub(last) < c.cfg.Cooldown {
		// Inside the window, a recorded failure means the checkpoint is down;
		// neither a lazy request nor a forced fill may retry it yet.
		if lastErr != nil && openedByFailure {
			return fmt.Errorf("%w (retrying in %s)", lastErr, c.cfg.Cooldown-now.Sub(last))
		}
		// The window was opened by a success. Every caller that got here is an
		// explicit replacement (the lazy "reuse a held cookie" decision lives
		// in staleLocked), so it proceeds; otherwise a challenge replay inside
		// the window would be silently skipped.
	}

	c.mu.Lock()
	c.lastAttempt = now
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	start := c.now()
	token, err := c.fetchChallenge(ctx)
	if errors.Is(err, errNoChallenge) {
		// Not challenged: no cookie is needed on this egress. Successful
		// acquisition with nothing to hold; every slot stays empty and the
		// probe result is what marks the ring as done.
		c.mu.Lock()
		for i := range c.slots {
			c.slots[i] = slot{}
		}
		c.probed = true
		c.probedAt = c.now()
		c.mu.Unlock()
		c.logf("clear: probe was not challenged; no clearance needed")
		return nil
	}
	if err != nil {
		return err
	}
	solution, err := solveToken(token)
	if err != nil {
		return fmt.Errorf("clear: solve: %w", err)
	}
	cookie, err := c.submitSolution(ctx, token, solution)
	if err != nil {
		return err
	}

	c.mu.Lock()
	if idx < 0 || idx >= len(c.slots) {
		idx = c.emptyLocked()
	}
	c.slots[idx] = slot{cookie: cookie, acquired: c.now(), refreshes: c.slots[idx].refreshes + 1}
	c.probed = false
	c.probedAt = time.Time{}
	c.refreshes++
	c.mu.Unlock()
	c.logf("clear: acquired %s into slot %d (solved in %s)", CookieName, idx, c.now().Sub(start).Round(time.Millisecond))
	return nil
}

// fetchChallenge GETs a protected path and extracts the challenge token from
// the response header. The header is authoritative; the 34 KiB HTML page is
// never parsed.
func (c *Clearance) fetchChallenge(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/session", nil)
	if err != nil {
		return "", fmt.Errorf("clear: build challenge request: %w", err)
	}
	browserHeaders(req)
	// A probe is about whether the edge challenges; a cookie that happens to
	// be in the shared client's jar would answer it with a 200 and mask the
	// challenge. Live acquisition never runs through a jar (the stdlib client
	// has none), but an injected one is possible.
	req.Header.Del("Cookie")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("clear: fetch challenge: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))

	token := resp.Header.Get("x-vercel-challenge-token")
	if resp.StatusCode == http.StatusOK {
		return "", errNoChallenge
	}
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusForbidden {
		return "", fmt.Errorf("clear: expected challenge, got HTTP %d", resp.StatusCode)
	}
	if token == "" {
		return "", errors.New("clear: challenge response carried no x-vercel-challenge-token")
	}
	return token, nil
}

// submitSolution POSTs the solved token. A 403 means the proof itself was
// rejected (ErrRefused); anything but 204/200 is a typed failure.
func (c *Clearance) submitSolution(ctx context.Context, token, solution string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+challengePath, nil)
	if err != nil {
		return "", fmt.Errorf("clear: build submission: %w", err)
	}
	req.Header.Set("user-agent", UserAgent)
	req.Header.Set("accept", "*/*")
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	req.Header.Set("origin", c.base)
	req.Header.Set("referer", c.base+"/.well-known/vercel/security/static/challenge.v2.min.js")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("x-vercel-challenge-token", token)
	req.Header.Set("x-vercel-challenge-solution", solution)
	req.Header.Set("x-vercel-challenge-version", "2")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("clear: submit solution: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))

	if resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("%w: the checkpoint rejected the proof of work (version drift?)", ErrRefused)
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("clear: solution rejected: HTTP %d", resp.StatusCode)
	}
	for _, ck := range resp.Cookies() {
		if strings.HasPrefix(ck.Name, "_vcrc") {
			return ck.Value, nil
		}
	}
	return "", errors.New("clear: no _vcrc* cookie in Set-Cookie")
}

// browserHeaders applies the navigation header set. Only the UA is required by
// the edge (the cookie is UA-bound); the rest is cheap realism.
func browserHeaders(req *http.Request) {
	req.Header.Set("user-agent", UserAgent)
	req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	req.Header.Set("sec-ch-ua", `"Not-A.Brand";v="24", "Chromium";v="141"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Linux"`)
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("sec-fetch-mode", "navigate")
	req.Header.Set("sec-fetch-user", "?1")
	req.Header.Set("sec-fetch-dest", "document")
	req.Header.Set("upgrade-insecure-requests", "1")
}

// TransportOptions configures the shared HTTP transport.
type TransportOptions struct {
	// Proxy is empty or an http(s) URL (config normalises bare host:port).
	Proxy string
	// RootCAs overrides the trust store (tests use a local CA).
	RootCAs *x509.CertPool
	// ResponseHeaderTimeout bounds the wait for response headers.
	ResponseHeaderTimeout time.Duration
	// DialTimeout bounds TCP connect (and proxy connect). Zero means 15s.
	DialTimeout time.Duration
}

// Transport returns the one stdlib transport every upstream request uses.
// HTTP/2 is mandatory: the clearance cookie is chained to the negotiated
// protocol, and h1.1 requests are answered with a fresh challenge even when
// the cookie is valid. Keep-alives stay on so h2 connections are reused.
func Transport(opts TransportOptions) *http.Transport {
	dialTimeout := opts.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 15 * time.Second
	}
	tr := &http.Transport{
		ForceAttemptHTTP2:     true,
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   8,
	}
	if opts.Proxy != "" {
		if u, err := url.Parse(opts.Proxy); err == nil && u.Host != "" {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	if opts.RootCAs != nil {
		tr.TLSClientConfig = &tls.Config{RootCAs: opts.RootCAs, MinVersion: tls.VersionTLS12}
	}
	return tr
}

// IsChallenge reports whether an upstream response is a Vercel checkpoint
// challenge, i.e. one a clearance refresh can fix. A 403 is deliberately
// excluded from the retry path: it means the proof itself was rejected.
func IsChallenge(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		return false
	}
	if strings.Contains(resp.Header.Get("x-vercel-mitigated"), "challenge") {
		return true
	}
	return resp.Header.Get("x-vercel-challenge-token") != ""
}
