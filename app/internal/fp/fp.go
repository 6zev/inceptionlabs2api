// Package fp builds the utls Chrome fingerprint transport used for every
// upstream connection. Vercel's bot challenge on chat.inceptionlabs.ai
// rejects HTTP/2 clients by JA4 on some networks, so the client hello is
// forced to advertise only http/1.1 via ALPN.
package fp

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
)

// NextProtos is the ALPN list advertised in every client hello. Keep it to
// http/1.1: adding h2 re-enables the fingerprint the challenge rejects.
var NextProtos = []string{"http/1.1"}

// Spec returns a fresh Chrome hello spec with ALPN pinned to http/1.1.
// A new spec is built per connection because ApplyPreset mutates it.
func Spec() (*utls.ClientHelloSpec, error) {
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		return nil, fmt.Errorf("fp: build chrome hello spec: %w", err)
	}
	ForceALPNH11(&spec)
	return &spec, nil
}

// ForceALPNH11 rewrites (or adds) the ALPN extension so it advertises only
// http/1.1.
func ForceALPNH11(spec *utls.ClientHelloSpec) {
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			alpn.AlpnProtocols = append([]string(nil), NextProtos...)
			return
		}
	}
	spec.Extensions = append(spec.Extensions, &utls.ALPNExtension{
		AlpnProtocols: append([]string(nil), NextProtos...),
	})
}

// Options configures the fingerprint transport.
type Options struct {
	// Proxy is empty, a bare host:port, or an http(s) URL.
	Proxy string
	// RootCAs overrides the system trust store. Tests use a local CA so a
	// TLS listener can be exercised end to end.
	RootCAs *x509.CertPool
}

// Transport returns an http.Transport whose TLS handshakes use the utls
// Chrome fingerprint. When Options.Proxy is non-empty the connection is
// tunnelled through an HTTP CONNECT proxy. Keep-alives stay disabled: the
// fingerprint bypass needs a fresh ClientHello per request, and each request
// already pays the handshake.
func Transport(opts Options) (*http.Transport, error) {
	dial, err := Dialer(opts)
	if err != nil {
		return nil, err
	}
	return &http.Transport{
		DialTLSContext:    dial,
		ForceAttemptHTTP2: false,
		DisableKeepAlives: true,
	}, nil
}

// Dialer returns a DialTLSContext function using opts.
func Dialer(opts Options) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	proxy, err := parseProxy(opts.Proxy)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialTLS(ctx, addr, proxy, opts.RootCAs)
	}, nil
}

type proxyAddr struct {
	host string
	// tlsProxy is true when the proxy itself is reached over TLS
	// (an https:// proxy URL).
	tlsProxy bool
}

func parseProxy(raw string) (*proxyAddr, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("fp: parse proxy %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("fp: unsupported proxy scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("fp: proxy %q has no host", raw)
	}
	return &proxyAddr{host: u.Host, tlsProxy: u.Scheme == "https"}, nil
}

func dialTLS(ctx context.Context, addr string, proxy *proxyAddr, roots *x509.CertPool) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("fp: split target %q: %w", addr, err)
	}

	var raw net.Conn
	dialer := &net.Dialer{}
	if proxy == nil {
		raw, err = dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("fp: dial %s: %w", addr, err)
		}
	} else {
		raw, err = dialer.DialContext(ctx, "tcp", proxy.host)
		if err != nil {
			return nil, fmt.Errorf("fp: dial proxy %s: %w", proxy.host, err)
		}
		if proxy.tlsProxy {
			tlsConn := tls.Client(raw, &tls.Config{
				ServerName: proxyHostname(proxy.host),
				MinVersion: tls.VersionTLS12,
			})
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, fmt.Errorf("fp: proxy tls handshake with %s: %w", proxy.host, err)
			}
			raw = tlsConn
		}
		tunnelled, err := connectTunnel(ctx, raw, addr)
		if err != nil {
			raw.Close()
			return nil, err
		}
		raw = tunnelled
	}

	spec, err := Spec()
	if err != nil {
		raw.Close()
		return nil, err
	}
	uconn := utls.UClient(raw, &utls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
		NextProtos: NextProtos,
		RootCAs:    roots,
	}, utls.HelloCustom)
	if err := uconn.ApplyPreset(spec); err != nil {
		raw.Close()
		return nil, fmt.Errorf("fp: apply chrome preset: %w", err)
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("fp: tls handshake with %s: %w", addr, err)
	}
	return uconn, nil
}

func proxyHostname(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// connectTunnel performs the HTTP CONNECT handshake through raw and returns
// a connection that preserves any TLS bytes the proxy sent ahead of the
// response head the reader consumed.
func connectTunnel(ctx context.Context, raw net.Conn, target string) (net.Conn, error) {
	if deadline, ok := ctx.Deadline(); ok {
		if err := raw.SetDeadline(deadline); err != nil {
			return nil, fmt.Errorf("fp: set CONNECT deadline: %w", err)
		}
		defer raw.SetDeadline(time.Time{})
	}
	if _, err := fmt.Fprintf(raw, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return nil, fmt.Errorf("fp: write CONNECT: %w", err)
	}
	br := bufio.NewReader(raw)
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("fp: read CONNECT response: %w", err)
	}
	fields := strings.SplitN(strings.TrimSpace(line), " ", 3)
	if len(fields) < 2 || fields[1] != "200" {
		return nil, fmt.Errorf("fp: proxy CONNECT to %s failed: %s", target, strings.TrimSpace(line))
	}
	for {
		header, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("fp: read CONNECT headers: %w", err)
		}
		if strings.TrimSpace(header) == "" {
			break
		}
	}
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: raw, r: br}, nil
	}
	return raw, nil
}

// bufferedConn preserves bytes a bufio.Reader read ahead from the proxy.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }
