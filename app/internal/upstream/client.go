package upstream

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"inceptionlabs2api/internal/clear"
	"inceptionlabs2api/internal/pool"
)

// ChromeUA is the browser user agent every upstream request carries. It is
// the same constant clearance acquisition uses: the _vcrcs cookie is bound to
// (egress IP, UA, HTTP/2), so the two must never diverge.
const ChromeUA = clear.UserAgent

// maxErrorBody caps how much of a non-200 upstream body is read (and logged).
const maxErrorBody = 4096

// maxAuthRetries is the number of additional attempts after the first when
// the upstream rejects a session token with 401/403.
const maxAuthRetries = 2

// StatusError is a typed non-200 upstream response. The body is kept for
// server-side logging and for a sanitised client-facing message.
type StatusError struct {
	Status int
	Body   string
}

// Error implements error.
func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream: status %d: %s", e.Status, truncate(e.Body, 200))
}

// ErrUnavailable means no session token could be obtained.
var ErrUnavailable = errors.New("upstream: no session token available")

// ErrAuthRejected means the upstream answered 401/403 on every attempt.
var ErrAuthRejected = errors.New("upstream: session token rejected")

// ErrClearance means the checkpoint could not be cleared.
var ErrClearance = errors.New("upstream: clearance unavailable")

// Client issues upstream requests through one shared HTTP/2 transport and the
// clearance-aware request path.
type Client struct {
	base    string
	pool    *pool.Pool
	timeout time.Duration
	logf    func(format string, args ...any)
	clear   *clear.Clearance
	hc      *http.Client
}

// Config configures a Client.
type Config struct {
	BaseURL string
	Proxy   string
	Pool    *pool.Pool
	// SessionTimeout bounds one /api/session fetch.
	SessionTimeout time.Duration
	// ClearanceRefresh is the proactive cookie refresh age. Zero means 45m.
	ClearanceRefresh time.Duration
	// ClearanceCooldown is the minimum spacing between acquisitions. Zero
	// means 15s.
	ClearanceCooldown time.Duration
	// RootCAs is optional; nil means the system trust store.
	RootCAs *x509.CertPool
	// Logf is optional; nil means log.Printf.
	Logf func(format string, args ...any)
}

// NewClient builds a Client and returns it together with a Fetcher suitable
// for pool.New. The transport is created once and shared by session fetches,
// chat, search, and clearance acquisition, so every request egresses through
// the same proxy with the same TLS configuration.
func NewClient(cfg Config) (*Client, pool.Fetcher) {
	if cfg.SessionTimeout <= 0 {
		cfg.SessionTimeout = 15 * time.Second
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	tr := clear.Transport(clear.TransportOptions{Proxy: cfg.Proxy, RootCAs: cfg.RootCAs})
	// No http.Client.Timeout: a wall-clock cap would kill legitimately long
	// diffusion generations. The sliding idle watchdog lives in Reader.
	hc := &http.Client{Transport: tr, CheckRedirect: checkRedirect}
	c := &Client{
		base:    strings.TrimRight(cfg.BaseURL, "/"),
		pool:    cfg.Pool,
		timeout: cfg.SessionTimeout,
		logf:    logf,
		hc:      hc,
	}
	c.clear = clear.New(clear.Config{
		BaseURL:  c.base,
		Client:   hc,
		Refresh:  cfg.ClearanceRefresh,
		Cooldown: cfg.ClearanceCooldown,
		Logf:     logf,
	})
	return c, c.FetchSession
}

// BindPool attaches the token pool the client draws from. It is called once
// at startup because the pool itself depends on the client's Fetcher.
func (c *Client) BindPool(p *pool.Pool) { c.pool = p }

// Logf returns the client's logger, used by the server for upstream diagnosis.
func (c *Client) Logf() func(format string, args ...any) { return c.logf }

// Clearance exposes the clearance state for /healthz.
func (c *Client) Clearance() *clear.Clearance { return c.clear }

// applyClearance sets the shared UA and the clearance cookie on req.
func (c *Client) applyClearance(req *http.Request) {
	ua, cookie := c.clear.Header()
	req.Header.Set("User-Agent", ua)
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
}

// ensureClearance acquires (or refreshes) the clearance cookie.
func (c *Client) ensureClearance(ctx context.Context) error {
	if err := c.clear.Ensure(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrClearance, err)
	}
	return nil
}

// Send performs an upstream request with clearance handling. prepare
// customises headers. On a 429 that carries a Vercel challenge, clearance is
// refreshed once and the request (with a fresh body reader, if any) is
// replayed. The caller owns the returned response body.
//
// This is the one request path: session fetches, chat, search, and probes all
// funnel through it, so no endpoint can accidentally bypass the cookie.
func (c *Client) Send(ctx context.Context, method, path string, body []byte, headerTimeout time.Duration, prepare func(*http.Request)) (*http.Response, error) {
	if err := c.ensureClearance(ctx); err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, method, path, body, headerTimeout, prepare)
	if err != nil {
		return nil, err
	}
	if !clear.IsChallenge(resp) {
		return resp, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	resp.Body.Close()
	c.logf("upstream: checkpoint challenged %s %s; refreshing clearance and replaying once", method, path)

	// The challenge proves the cookie is not accepted; forget it rather than
	// letting the cooldown reuse it, then acquire a new one.
	c.clear.Invalidate()
	if err := c.clear.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrClearance, err)
	}
	return c.do(ctx, method, path, body, headerTimeout, prepare)
}

// do issues one upstream request. A nil body means no request body.
// headerTimeout bounds the whole pre-response phase (dial, TLS, headers); the
// response body is not subject to it, so long generations are unaffected.
func (c *Client) do(ctx context.Context, method, path string, body []byte, headerTimeout time.Duration, prepare func(*http.Request)) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	if headerTimeout <= 0 {
		headerTimeout = 60 * time.Second
	}
	hctx, cancelHeader := context.WithCancel(ctx)
	timer := time.AfterFunc(headerTimeout, cancelHeader)

	req, err := http.NewRequestWithContext(hctx, method, c.base+path, rdr)
	if err != nil {
		timer.Stop()
		cancelHeader()
		return nil, fmt.Errorf("upstream: build %s request: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.applyClearance(req)
	if prepare != nil {
		prepare(req)
	}
	resp, err := c.hc.Do(req)
	timer.Stop()
	if err != nil {
		timedOut := hctx.Err() != nil && ctx.Err() == nil
		cancelHeader()
		if timedOut {
			return nil, fmt.Errorf("%w: no response headers within %s", ErrIdleTimeout, headerTimeout)
		}
		return nil, fmt.Errorf("upstream: %s request: %w", path, err)
	}
	resp.Body = &closeHook{ReadCloser: resp.Body, hook: cancelHeader}
	return resp, nil
}

// closeHook runs hook once when the body is closed, releasing the per-request
// header timer context.
type closeHook struct {
	io.ReadCloser
	hook func()
}

func (c *closeHook) Close() error {
	err := c.ReadCloser.Close()
	c.hook()
	return err
}

// FetchSession calls GET /api/session once and validates the token shape.
func (c *Client) FetchSession(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := c.Send(ctx, http.MethodGet, "/api/session", nil, c.timeout, func(req *http.Request) {
		req.Header.Set("Accept", "*/*")
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("upstream: read session response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", &StatusError{Status: resp.StatusCode, Body: string(body)}
	}
	var p pool.Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return "", fmt.Errorf("upstream: parse session response: %w (body %q)", err, truncate(string(body), 200))
	}
	if !p.OK || p.Token == "" {
		return "", fmt.Errorf("upstream: session response ok=%v token present=%v", p.OK, p.Token != "")
	}
	if !pool.ValidateToken(p.Token) {
		return "", fmt.Errorf("upstream: malformed session token %q", truncate(p.Token, 16))
	}
	return p.Token, nil
}

// checkRedirect rejects redirects that leave the configured origin or
// downgrade HTTPS. The session token travels as a custom header, which Go
// would otherwise copy to a cross-host redirect target. The clearance cookie
// is scoped to the origin for the same reason.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return fmt.Errorf("upstream: stopped after %d redirects", len(via))
	}
	orig := via[0].URL
	if req.URL.Host != orig.Host {
		return fmt.Errorf("upstream: refusing cross-host redirect to %s", req.URL.Host)
	}
	if orig.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("upstream: refusing HTTPS downgrade on redirect")
	}
	return nil
}

// Stream is an opened upstream SSE stream. Close releases the response body.
type Stream struct {
	Body   io.ReadCloser
	Reader *Reader
}

// Close closes the response body.
func (s *Stream) Close() error { return s.Body.Close() }

// Chat opens a stream for req. It acquires a token, POSTs, and retries with a
// fresh token on 401/403, at most maxAuthRetries times. It returns a typed
// *StatusError for other non-200 responses and an error wrapping
// ErrUnavailable when the pool cannot supply a token. Client cancellation and
// transport failures never discard a token.
func (c *Client) Chat(ctx context.Context, req *ChatRequest, idle time.Duration, onMalformed func(string)) (*Stream, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("upstream: marshal chat request: %w", err)
	}
	resp, err := c.postAuthed(ctx, "/api/chat", "chat", body, idle, func(req *http.Request) {
		req.Header.Set("Accept", "text/event-stream, application/json")
	})
	if err != nil {
		return nil, err
	}
	return &Stream{
		Body:   resp.Body,
		Reader: NewReader(resp.Body, idle, onMalformed),
	}, nil
}

// postAuthed POSTs body to path with a pooled token, retrying with a fresh
// token on 401/403 at most maxAuthRetries times. It returns a 200 response
// whose body the caller owns. A 401/403 discards that token; other non-200
// statuses are a typed *StatusError and keep the token, since the pool never
// poisons a token for an upstream fault. prepare may customise headers.
func (c *Client) postAuthed(ctx context.Context, path, label string, body []byte, headerTimeout time.Duration, prepare func(*http.Request)) (*http.Response, error) {
	if c.pool == nil {
		return nil, fmt.Errorf("%w: no pool bound", ErrUnavailable)
	}
	var lastErr error
	for attempt := 0; attempt <= maxAuthRetries; attempt++ {
		token, err := c.pool.Get(ctx)
		if err != nil {
			// A pool that could not fill because the checkpoint refused us is
			// a clearance failure, not a generic "no token": the operator can
			// act on it (egress, UA, PoW drift).
			if errors.Is(err, ErrClearance) {
				return nil, fmt.Errorf("%w: %w", ErrClearance, err)
			}
			return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}
		resp, err := c.Send(ctx, http.MethodPost, path, body, headerTimeout, func(req *http.Request) {
			req.Header.Set("x-session-token", token)
			if prepare != nil {
				prepare(req)
			}
		})
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return resp, nil
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
			resp.Body.Close()
			c.pool.Discard(token)
			c.logf("upstream: session token rejected (status %d), retrying", resp.StatusCode)
			lastErr = fmt.Errorf("%w: status %d", ErrAuthRejected, resp.StatusCode)
			continue
		default:
			bodyText, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
			resp.Body.Close()
			c.logf("upstream: %s status %d: %s", label, resp.StatusCode, truncate(string(bodyText), 500))
			return nil, &StatusError{Status: resp.StatusCode, Body: string(bodyText)}
		}
	}
	return nil, lastErr
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
