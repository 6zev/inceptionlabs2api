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

	"inceptionlabs2api/internal/fp"
	"inceptionlabs2api/internal/pool"
)

// ChromeUA is the browser user agent the upstream expects for anonymous
// sessions. It must stay consistent between /api/session and /api/chat.
const ChromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// maxErrorBody caps how much of a non-200 upstream body is read (and logged).
const maxErrorBody = 4096

// maxAuthRetries is the number of additional attempts after the first when
// the upstream rejects a session token with 401/403.
const maxAuthRetries = 2

// StatusError is a typed non-200 upstream response. The body is kept for
// server-side logging and never sent to clients verbatim.
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

// Client issues /api/chat requests through the fingerprint transport.
type Client struct {
	base    string
	proxy   string
	pool    *pool.Pool
	roots   *x509.CertPool
	timeout time.Duration
	logf    func(format string, args ...any)
}

// Config configures a Client.
type Config struct {
	BaseURL string
	Proxy   string
	Pool    *pool.Pool
	// SessionTimeout bounds one /api/session fetch.
	SessionTimeout time.Duration
	// RootCAs is optional; nil means the system trust store.
	RootCAs *x509.CertPool
	// Logf is optional; nil means log.Printf.
	Logf func(format string, args ...any)
}

// NewClient builds a Client and returns it together with a Fetcher suitable
// for pool.New.
func NewClient(cfg Config) (*Client, pool.Fetcher) {
	if cfg.SessionTimeout <= 0 {
		cfg.SessionTimeout = 15 * time.Second
	}
	logf := cfg.Logf
	if logf == nil {
		logf = log.Printf
	}
	c := &Client{
		base:    strings.TrimRight(cfg.BaseURL, "/"),
		proxy:   cfg.Proxy,
		pool:    cfg.Pool,
		roots:   cfg.RootCAs,
		timeout: cfg.SessionTimeout,
		logf:    logf,
	}
	return c, c.FetchSession
}

// Logf returns the client's logger, used by the server for upstream diagnosis.
func (c *Client) Logf() func(format string, args ...any) { return c.logf }

// FetchSession calls GET /api/session once and validates the token shape.
func (c *Client) FetchSession(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	tr, err := fp.Transport(fp.Options{Proxy: c.proxy, RootCAs: c.roots})
	if err != nil {
		return "", err
	}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/session", nil)
	if err != nil {
		return "", fmt.Errorf("upstream: build session request: %w", err)
	}
	req.Header.Set("User-Agent", ChromeUA)
	req.Header.Set("Accept", "*/*")

	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("upstream: fetch session: %w", err)
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

// Stream is an opened upstream SSE stream. Close releases the connection and
// the per-request transport.
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

	var lastErr error
	for attempt := 0; attempt <= maxAuthRetries; attempt++ {
		token, err := c.pool.Get(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		resp, err := c.post(ctx, token, body)
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return &Stream{
				Body:   resp.Body,
				Reader: NewReader(resp.Body, idle, onMalformed),
			}, nil
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
			c.logf("upstream: chat status %d: %s", resp.StatusCode, truncate(string(bodyText), 500))
			return nil, &StatusError{Status: resp.StatusCode, Body: string(bodyText)}
		}
	}
	return nil, lastErr
}

func (c *Client) post(ctx context.Context, token string, body []byte) (*http.Response, error) {
	tr, err := fp.Transport(fp.Options{Proxy: c.proxy, RootCAs: c.roots})
	if err != nil {
		return nil, err
	}
	// No http.Client.Timeout: a wall-clock cap would kill legitimately long
	// diffusion generations. The sliding idle watchdog lives in Reader.
	hc := &http.Client{Transport: tr}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/chat", bytes.NewReader(body))
	if err != nil {
		tr.CloseIdleConnections()
		return nil, fmt.Errorf("upstream: build chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-session-token", token)
	req.Header.Set("User-Agent", ChromeUA)
	req.Header.Set("Accept", "text/event-stream, application/json")

	resp, err := hc.Do(req)
	if err != nil {
		tr.CloseIdleConnections()
		return nil, fmt.Errorf("upstream: chat request: %w", err)
	}
	// Release the per-request transport when the body is closed.
	resp.Body = &closeHook{ReadCloser: resp.Body, hook: tr.CloseIdleConnections}
	return resp, nil
}

// closeHook runs hook once when the body is closed, releasing the
// per-request transport.
type closeHook struct {
	io.ReadCloser
	hook func()
}

func (c *closeHook) Close() error {
	err := c.ReadCloser.Close()
	c.hook()
	return err
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
