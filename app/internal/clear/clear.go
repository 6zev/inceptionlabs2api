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

// State is the /healthz view of clearance.
type State struct {
	HasCookie    bool  `json:"has_cookie"`
	AgeSeconds   int64 `json:"age_seconds"`
	RefreshCount int64 `json:"refresh_count"`
}

// Config configures a Clearance.
type Config struct {
	// BaseURL is the upstream origin, e.g. https://chat.inceptionlabs.ai.
	BaseURL string
	// Client is the HTTP client acquisition uses. It must share the egress
	// (proxy) the gateway's other requests use: the cookie is IP-bound.
	Client *http.Client
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

// Clearance holds the current cookie. It is safe for concurrent use; refresh
// work is single-flight.
type Clearance struct {
	cfg    Config
	base   string
	client *http.Client
	now    func() time.Time
	logf   func(format string, args ...any)

	mu       sync.Mutex
	cookie   string
	acquired time.Time
	// probed records a successful "this egress is not challenged" result.
	// Without it Ensure would perform the /api/session probe before every
	// upstream request, doubling the session traffic the pool already makes.
	probed bool

	refreshes   int64
	lastAttempt time.Time
	lastErr     error

	inflight     bool
	inflightDone chan struct{}
}

// New builds a Clearance. A nil Client means a client over a default stdlib
// transport, which is enough for tests but not for a proxy setup.
func New(cfg Config) *Clearance {
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
	}
}

// Header returns the UA and the Cookie header value to send. The cookie is
// empty when none has been acquired yet.
func (c *Clearance) Header() (ua, cookie string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cookie == "" {
		return UserAgent, ""
	}
	return UserAgent, CookieName + "=" + c.cookie
}

// Cookie returns the clearance cookie value alone ("" when none).
func (c *Clearance) Cookie() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cookie
}

// State reports clearance health for /healthz.
func (c *Clearance) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := State{HasCookie: c.cookie != "", RefreshCount: c.refreshes}
	if st.HasCookie {
		age := c.now().Sub(c.acquired)
		if age < 0 {
			age = 0
		}
		st.AgeSeconds = int64(age.Seconds())
	}
	return st
}

// staleLocked reports whether the clearance state is past the refresh margin.
// An unchallenged egress holds no cookie but is still fresh for one refresh
// window, for the same reason a cookie is.
func (c *Clearance) staleLocked() bool {
	if c.cookie == "" {
		return !c.probed || c.now().Sub(c.acquired) >= c.cfg.Refresh
	}
	return c.now().Sub(c.acquired) >= c.cfg.Refresh
}

// Ensure acquires a cookie if none is held or the held one is past the
// refresh margin. Concurrent callers coalesce into one acquisition.
func (c *Clearance) Ensure(ctx context.Context) error {
	c.mu.Lock()
	needed := c.staleLocked()
	c.mu.Unlock()
	if !needed {
		return nil
	}
	return c.acquire(ctx)
}

// Refresh replaces the cookie, subject to the cooldown. It is called when the
// edge challenged a request that already carried a cookie. Inside the
// cooldown the held cookie is reused rather than solved again; with no cookie
// at all an acquisition always proceeds, otherwise nothing could recover.
func (c *Clearance) Refresh(ctx context.Context) error {
	return c.acquire(ctx)
}

// Prewarm starts one clearance acquisition in the background and returns
// immediately. Boot calls it so the first request does not pay the solve and
// network round trip (SPEC §2.1.1); a failure is logged and left to the
// ordinary lazy path (Ensure on the next request, Refresh on a challenge).
// The goroutine is bounded by the acquisition timeout it inherits.
func (c *Clearance) Prewarm(ctx context.Context) {
	go func() {
		if err := c.Ensure(ctx); err != nil {
			c.logf("clear: prewarm failed (requests will retry lazily): %v", err)
		}
	}()
}

// RunRefreshLoop keeps a held cookie fresh from boot, checking at every tick
// whether it has reached the configured refresh age and replacing it if so.
// It only maintains a cookie it already holds: when none exists (an egress
// that is never challenged, or a failed prewarm) it does nothing, because
// Ensure is the path that retries acquisition and prewarm already ran once.
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
			// Ensure is a no-op while the cookie is fresh; it refreshes
			// exactly when the age reaches the margin, from boot onwards.
			if c.Cookie() == "" {
				continue
			}
			if err := c.Ensure(ctx); err != nil {
				c.logf("clear: proactive refresh failed: %v", err)
			}
		}
	}
}

// Invalidate forgets the cookie so the next Ensure acquires a fresh one.
func (c *Clearance) Invalidate() {
	c.mu.Lock()
	c.cookie = ""
	c.probed = false
	c.mu.Unlock()
}

// acquire runs at most one refresh at a time, no matter how many callers.
func (c *Clearance) acquire(ctx context.Context) error {
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

	err := c.refresh(ctx)

	c.mu.Lock()
	c.inflight = false
	c.inflightDone = nil
	if err != nil {
		c.lastErr = err
	} else {
		c.lastErr = nil
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
func (c *Clearance) refresh(ctx context.Context) error {
	now := c.now()
	c.mu.Lock()
	last := c.lastAttempt
	has := c.cookie != ""
	lastErr := c.lastErr
	c.mu.Unlock()

	if c.cfg.Cooldown > 0 && !last.IsZero() && now.Sub(last) < c.cfg.Cooldown {
		if has {
			return nil
		}
		if lastErr != nil {
			return fmt.Errorf("%w (retrying in %s)", lastErr, c.cfg.Cooldown-now.Sub(last))
		}
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
		// acquisition with nothing to hold.
		c.mu.Lock()
		c.cookie = ""
		c.acquired = c.now()
		c.probed = true
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
	c.cookie = cookie
	c.acquired = c.now()
	c.probed = false
	c.refreshes++
	c.mu.Unlock()
	c.logf("clear: acquired %s (solved in %s)", CookieName, c.now().Sub(start).Round(time.Millisecond))
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
