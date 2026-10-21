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
	"math/rand"
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
	// DefaultCooldown is the per-slot minimum spacing after a *failed*
	// acquisition. A burst of challenged requests must not turn into a solve
	// storm, while the loop keeps working the other slots.
	DefaultCooldown = 15 * time.Second
	// DefaultTimeout bounds one acquisition (GET + solve + POST).
	DefaultTimeout = 30 * time.Second
	// DefaultPool is the ring size, matching the anonymous-stock shape of the
	// chatglm2api identity pool (§2.1.2): a challenged cookie is disposable
	// and losing one must not cost a stall.
	DefaultPool = 20
	// DefaultFillPace is the pause the fill loop takes between acquisitions.
	// Together with the jitter it spreads a 20-slot fill over 30-60 s: the
	// edge sees a mild trickle, not a burst (SPEC §2.1.2).
	DefaultFillPace = 1500 * time.Millisecond
	// DefaultJitter is the extra random pause added to FillPace, so the
	// acquisitions are not a metronome.
	DefaultJitter = 500 * time.Millisecond
	// maxBodyBytes drops what we read from challenge responses; only headers
	// and cookies matter.
	maxBodyBytes = 1 << 20
)

// SlotState is the state of one ring slot (SPEC §2.1.2).
type SlotState string

const (
	// SlotPending means the slot has never been filled, or was retired by a
	// challenge and waits for the fill loop.
	SlotPending SlotState = "pending"
	// SlotLive means the slot holds a cookie.
	SlotLive SlotState = "live"
	// SlotCooldown means the slot's last acquisition failed and it waits out
	// the per-slot cooldown before the loop retries it.
	SlotCooldown SlotState = "cooldown"
)

// ErrRefused means the edge answered the submission with 403: the proof of
// work itself was rejected (checkpoint version drift). Retrying cannot help.
var ErrRefused = errors.New("clear: challenge solution refused")

// errNoChallenge means the edge answered the probe without a challenge, so
// there is nothing to clear: this egress is already welcome.
var errNoChallenge = errors.New("clear: no challenge issued")

// State is the /healthz view of clearance. Live/Pool describe the ring as a
// whole (HasCookie = any live slot, AgeSeconds = the oldest live slot);
// Slots reports each configured slot, so an operator can watch the ring fill
// to 20/20 and see one retired slot against a healthy ring (SPEC §2.1.2).
type State struct {
	Live         int        `json:"live"`
	Pool         int        `json:"pool"`
	HasCookie    bool       `json:"has_cookie"`
	AgeSeconds   int64      `json:"age_seconds"`
	RefreshCount int64      `json:"refresh_count"`
	Slots        []SlotView `json:"slots"`
}

// SlotView is the health view of one ring slot.
type SlotView struct {
	Cookie       bool      `json:"cookie"`
	State        SlotState `json:"state"`
	AgeSeconds   int64     `json:"age_seconds"`
	RefreshCount int64     `json:"refresh_count"`
}

// slot is one clearance cookie in the rotation ring, plus the per-slot
// bookkeeping of its lifecycle.
type slot struct {
	cookie    string
	acquired  time.Time
	refreshes int64

	// lastAttempt and lastErr implement the per-slot failure cooldown: only a
	// slot whose own attempt failed waits, so one broken attempt cannot freeze
	// the fills or the serving of the others.
	lastAttempt time.Time
	failed      bool
	lastErr     error
}

// stateLocked reports the slot's lifecycle state.
func (s *slot) stateLocked(now time.Time, cooldown time.Duration) SlotState {
	if s.cookie != "" {
		return SlotLive
	}
	if s.failed && cooldown > 0 && !s.lastAttempt.IsZero() && now.Sub(s.lastAttempt) < cooldown {
		return SlotCooldown
	}
	return SlotPending
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
	// Cooldown is the per-slot minimum spacing after a failed acquisition.
	// Zero means DefaultCooldown; negative disables it (tests).
	Cooldown time.Duration
	// FillPace is the pause the fill loop takes between acquisitions. Zero
	// means DefaultFillPace; negative disables it (tests).
	FillPace time.Duration
	// Jitter is the extra random pause added to FillPace. Zero means
	// DefaultJitter; negative disables it (tests).
	Jitter time.Duration
	// Timeout bounds one acquisition. Zero means DefaultTimeout.
	Timeout time.Duration
	// Now is injectable for tests. Nil means time.Now.
	Now func() time.Time
	// Logf is optional; nil discards.
	Logf func(format string, args ...any)
}

// Clearance holds a ring of cookies. It is safe for concurrent use. All
// acquisition work runs in at most one place: the request path may perform a
// single lazy Ensure, and one background fill loop maintains the rest
// (§2.1.2).
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

	refreshes int64

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
	if cfg.FillPace == 0 {
		cfg.FillPace = DefaultFillPace
	}
	if cfg.FillPace < 0 {
		cfg.FillPace = 0
	}
	if cfg.Jitter == 0 {
		cfg.Jitter = DefaultJitter
	}
	if cfg.Jitter < 0 {
		cfg.Jitter = 0
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
	st := State{
		Pool:         len(c.slots),
		RefreshCount: c.refreshes,
		Slots:        make([]SlotView, len(c.slots)),
	}
	oldest := time.Duration(-1)
	for i := range c.slots {
		s := &c.slots[i]
		ss := SlotView{
			Cookie:       s.cookie != "",
			State:        s.stateLocked(now, c.cfg.Cooldown),
			RefreshCount: s.refreshes,
		}
		if s.cookie != "" {
			st.Live++
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
// margin when every slot is stale. It is the lazy request path and never
// waits for a fill that is already running; the per-request path never tops the
// ring up — the background loop does that (SPEC §2.1.2).
func (c *Clearance) Ensure(ctx context.Context) error {
	c.mu.Lock()
	needed := c.staleLocked()
	idx := c.emptyLocked()
	c.mu.Unlock()
	if !needed {
		return nil
	}
	return c.acquireSlot(ctx, idx)
}

// Refresh replaces the cookie a challenge just rejected, or acquires one when
// none is held. It is the recovery path: a window opened by a failure still
// spaces it, but it always attempts a solve when no healthy replay is
// possible.
func (c *Clearance) Refresh(ctx context.Context) error {
	c.mu.Lock()
	idx := c.emptyLocked()
	c.mu.Unlock()
	return c.acquireSlot(ctx, idx)
}

// PrewarmBoot fills the first slot synchronously and returns when it is live
// (SPEC §2.1.1, §2.1.2 — "warm a base, not the maximum"). Boot calls it, then
// starts the server: one slot is enough to serve. An error is returned but is
// NOT a failed startup: the caller logs it, serves `has_cookie:false`, and
// StartFillLoop retries in the background.
func (c *Clearance) PrewarmBoot(ctx context.Context) error {
	idx := c.firstEmpty()
	err := c.acquireSlot(ctx, idx)
	if err != nil {
		return err
	}
	if c.cfg.Pool > 1 {
		c.logf("clear: boot slot live; the remaining %d slots fill in the background", c.cfg.Pool-1)
	}
	return nil
}

// PrewarmNow fills the whole ring sequentially and reports the first failure.
// It is the blocking variant used by tests and by callers that want a full
// ring before proceeding; boot uses PrewarmBoot instead. It does not apply
// the loop's FillPace pause — the caller asked to wait for the full ring.
func (c *Clearance) PrewarmNow(ctx context.Context) error {
	var firstErr error
	for {
		if c.liveLocked() >= c.cfg.Pool || c.probedLocked() {
			break
		}
		idx := c.firstEmpty()
		if idx < 0 {
			break
		}
		if err := c.acquireSlot(ctx, idx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// StartFillLoop runs the one background goroutine that maintains the ring: it
// fills a pending (never-filled or retired) slot, or replaces the oldest slot
// past the refresh margin — one acquisition per iteration — then pauses
// FillPace + jitter (SPEC §2.1.2). Filling and refreshing share the loop so a
// 429's refill and the 45-minute rotation are the same code path, and so the
// edge never sees two acquisitions at once.
//
// It returns a channel that is closed when the loop has exited, which makes
// shutdown observable (the process does not leak the goroutine). Cancelling
// ctx stops it promptly, including during a paced pause.
func (c *Clearance) StartFillLoop(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.fillLoop(ctx)
	}()
	return done
}

// fillLoop is the body of StartFillLoop.
func (c *Clearance) fillLoop(ctx context.Context) {
	for {
		filled := c.fillOnce(ctx)
		if !c.sleep(ctx, c.nextPause(filled)) {
			return
		}
	}
}

// nextPause is the idle time after one iteration: FillPace + jitter after a
// fill (so consecutive acquisitions are paced), and a second FillPace — at
// least a second — when there was nothing to do, so an idle ring is still
// checked often enough to react to a 429 promptly.
func (c *Clearance) nextPause(filled bool) time.Duration {
	pause := c.cfg.FillPace + c.jitter()
	if !filled {
		pause += c.cfg.FillPace
		if pause < time.Second {
			pause = time.Second
		}
	}
	return pause
}

// fillOnce performs at most one acquisition: the lowest-numbered pending slot
// first, else the oldest stale slot. It reports whether it filled any slot.
// An unchallenged egress (probed, no cookies) is left alone.
func (c *Clearance) fillOnce(ctx context.Context) bool {
	c.mu.Lock()
	if c.probed && c.liveLocked() == 0 {
		c.mu.Unlock()
		return false
	}
	idx := c.fillTargetLocked(c.now())
	c.mu.Unlock()
	if idx < 0 {
		return false
	}
	if err := c.acquireSlot(ctx, idx); err != nil {
		c.logf("clear: background fill of slot %d failed: %v", idx, err)
	}
	return true
}

// fillTargetLocked chooses the slot one iteration should work on: the first
// pending slot (a retired one first, so a 429's gap is repaired before the
// initial fill on a fresh process), then the oldest stale slot, else -1.
func (c *Clearance) fillTargetLocked(now time.Time) int {
	for i := range c.slots {
		// A pending slot in its failure cooldown is not a target yet: the loop
		// moves on and returns to it when the window closes. Without this a
		// refusing checkpoint would make the loop spin on slot 0 forever.
		if c.slots[i].stateLocked(now, c.cfg.Cooldown) == SlotPending {
			return i
		}
	}
	idx, oldest := -1, time.Duration(0)
	for i := range c.slots {
		age := now.Sub(c.slots[i].acquired)
		if age < c.cfg.Refresh {
			continue
		}
		if idx < 0 || age > oldest {
			idx, oldest = i, age
		}
	}
	return idx
}

// RefreshStale replaces at most one slot that has reached the refresh age.
// The fill loop calls it once per iteration; the per-tick budget is one, so a
// 20-slot ring rotates inside the 45m→60m headroom with no burst (§2.1.2).
func (c *Clearance) RefreshStale(ctx context.Context) error {
	c.mu.Lock()
	idx, have := c.staleSlotLocked()
	c.mu.Unlock()
	if !have {
		return nil
	}
	return c.acquireSlot(ctx, idx)
}

// jitter returns a random pause in [0, Jitter).
func (c *Clearance) jitter() time.Duration {
	if c.cfg.Jitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(c.cfg.Jitter)))
}

// sleep waits for d or for ctx cancellation; it reports whether the wait
// completed (false means cancelled).
func (c *Clearance) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// firstEmpty returns the index of the first empty slot, or -1 when the ring
// has no empty slot.
func (c *Clearance) firstEmpty() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.slots {
		if c.slots[i].cookie == "" {
			return i
		}
	}
	return -1
}

// probedLocked reports the recorded "this egress is not challenged" result,
// which only means anything while no slot is live.
func (c *Clearance) probedLocked() bool {
	return c.probed && c.liveLocked() == 0
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
// is retired. The call is O(ring) and never solves: it runs on the request
// path, whose caller immediately replays on the next live slot. The fill loop
// notices the shortfall and refills the slot (SPEC §2.1.2). An unknown value
// is a no-op.
func (c *Clearance) Retire(cookie string) error {
	cookie = strings.TrimPrefix(cookie, CookieName+"=")
	c.mu.Lock()
	for i := range c.slots {
		if c.slots[i].cookie == cookie {
			// Drop only the cookie. The slot's cooldown bookkeeping belongs to
			// a possible in-flight acquisition and must survive.
			c.slots[i].cookie = ""
			c.slots[i].acquired = time.Time{}
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

// Refill acquires one cookie into an empty slot, or replaces the oldest slot
// when the ring is full. It is the blocking primitive the fill loop's work
// amounts to; the 429 path does not call it (the loop does).
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
// slot idx and recording the outcome on that slot. It is the existing
// single-flight: the fill is sequential anyway, so it costs nothing, and it
// keeps lazy request-path Ensure calls from stacking solves behind the fill
// (§2.1.2 — zero concurrency).
func (c *Clearance) acquireSlot(ctx context.Context, idx int) error {
	c.mu.Lock()
	if c.inflight {
		done := c.inflightDone
		c.mu.Unlock()
		return c.wait(ctx, idx, done)
	}
	c.inflight = true
	c.inflightDone = make(chan struct{})
	done := c.inflightDone
	c.mu.Unlock()

	err := c.refresh(ctx, idx)

	c.mu.Lock()
	c.inflight = false
	c.inflightDone = nil
	if idx >= 0 && idx < len(c.slots) {
		c.slots[idx].lastAttempt = c.now()
		c.slots[idx].failed = err != nil
		c.slots[idx].lastErr = err
	}
	c.mu.Unlock()
	close(done)
	return err
}

// wait blocks until the owner acquisition finishes and reports its result.
func (c *Clearance) wait(ctx context.Context, idx int, done chan struct{}) error {
	select {
	case <-done:
		c.mu.Lock()
		defer c.mu.Unlock()
		// A successful acquisition may legitimately end with no cookie: an
		// unchallenged egress needs none. Only a recorded error is a failure.
		if idx >= 0 && idx < len(c.slots) {
			return c.slots[idx].lastErr
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// refresh runs the GET -> solve -> POST flow, honouring the slot's cooldown.
//
// The cooldown bounds *failed* attempts on ONE slot: a burst of requests
// against a checkpoint that keeps refusing must not become a solve storm
// (SPEC §2.1.1), but it must not stop the other slots from filling or serving
// either. A caller without a cookie gets that slot's recorded error back
// rather than a silent success that would send an unprotected request. A
// successful acquisition never opens a window, so recovery — a 429 challenge
// right after a solve — is never blocked.
func (c *Clearance) refresh(ctx context.Context, idx int) error {
	if idx < 0 || idx >= len(c.slots) {
		idx = c.firstEmpty()
	}
	now := c.now()
	c.mu.Lock()
	last := c.slots[idx].lastAttempt
	lastErr := c.slots[idx].lastErr
	openedByFailure := c.slots[idx].failed
	c.mu.Unlock()

	if c.cfg.Cooldown > 0 && !last.IsZero() && now.Sub(last) < c.cfg.Cooldown {
		// Inside this slot's window, a recorded failure means the checkpoint is
		// down; neither a lazy request nor a fill may retry it yet.
		if lastErr != nil && openedByFailure {
			return fmt.Errorf("%w (retrying in %s)", lastErr, c.cfg.Cooldown-now.Sub(last))
		}
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	// Stamp the attempt before the network call, so a failed attempt opens its
	// cooldown even if the process is busy for the whole timeout.
	c.mu.Lock()
	if idx >= 0 && idx < len(c.slots) {
		c.slots[idx].lastAttempt = now
	}
	c.mu.Unlock()

	start := c.now()
	token, err := c.fetchChallenge(ctx)
	if errors.Is(err, errNoChallenge) {
		// Not challenged: no cookie is needed on this egress. Successful
		// acquisition with nothing to hold; every slot stays empty and the
		// probe result is what tells the fill loop to stop probing.
		c.mu.Lock()
		for i := range c.slots {
			// Drop only the cookies: cooldown bookkeeping belongs to the slot
			// that recorded it.
			c.slots[i].cookie = ""
			c.slots[i].acquired = time.Time{}
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
	c.slots[idx].cookie = cookie
	c.slots[idx].acquired = c.now()
	c.slots[idx].refreshes++
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
