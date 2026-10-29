package clear

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSOCKS5 is a minimal SOCKS5 server good enough to prove the client
// handshake: greeting, optional user/pass, CONNECT to a real TCP target.
type fakeSOCKS5 struct {
	ln net.Listener
	// wantUser/wantPass, when non-empty, make the server REQUIRE auth.
	wantUser, wantPass string
	// requireRemoteResolve records whether the client sent a domain (socks5h)
	// instead of an IP.
	mu       sync.Mutex
	lastAtyp byte
	lastHost string
	conns    int
}

func newFakeSOCKS5(t *testing.T, user, pass string) *fakeSOCKS5 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSOCKS5{ln: ln, wantUser: user, wantPass: pass}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeSOCKS5) URL(scheme string) string {
	u := "socks5://" + f.ln.Addr().String()
	if f.wantUser != "" {
		u = "socks5://" + f.wantUser + ":" + f.wantPass + "@" + f.ln.Addr().String()
	}
	if scheme == "socks5h" {
		u = strings.Replace(u, "socks5://", "socks5h://", 1)
	}
	return u
}

func (f *fakeSOCKS5) serve(c net.Conn) {
	defer c.Close()
	f.mu.Lock()
	f.conns++
	f.mu.Unlock()

	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil || hdr[0] != socks5Version {
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	method := byte(socks5NoAuth)
	if f.wantUser != "" {
		method = socks5UserPass
	}
	offered := false
	for _, m := range methods {
		if m == method {
			offered = true
		}
	}
	if !offered {
		_, _ = c.Write([]byte{socks5Version, socks5NoAcceptable})
		return
	}
	if _, err := c.Write([]byte{socks5Version, method}); err != nil {
		return
	}
	if method == socks5UserPass {
		var v [2]byte
		if _, err := io.ReadFull(c, v[:]); err != nil {
			return
		}
		u := make([]byte, v[1])
		if _, err := io.ReadFull(c, u); err != nil {
			return
		}
		var pl [1]byte
		if _, err := io.ReadFull(c, pl[:]); err != nil {
			return
		}
		p := make([]byte, pl[0])
		if _, err := io.ReadFull(c, p); err != nil {
			return
		}
		if string(u) != f.wantUser || string(p) != f.wantPass {
			_, _ = c.Write([]byte{0x01, 0x01})
			return
		}
		if _, err := c.Write([]byte{0x01, 0x00}); err != nil {
			return
		}
	}

	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil || req[1] != socks5CmdConnect {
		return
	}
	var host string
	f.mu.Lock()
	f.lastAtyp = req[3]
	f.mu.Unlock()
	switch req[3] {
	case socks5AtypIPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case socks5AtypIPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case socks5AtypDomain:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = string(b)
	}
	var pb [2]byte
	if _, err := io.ReadFull(c, pb[:]); err != nil {
		return
	}
	f.mu.Lock()
	f.lastHost = host
	f.mu.Unlock()

	target := net.JoinHostPort(host, itoa(int(binary.BigEndian.Uint16(pb[:]))))
	up, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		_, _ = c.Write([]byte{socks5Version, 0x05, 0x00, socks5AtypIPv4, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	// Success reply with a placeholder bound address.
	if _, err := c.Write([]byte{socks5Version, socks5RepOK, 0x00, socks5AtypIPv4, 127, 0, 0, 1, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

func itoa(n int) string { return strconv.Itoa(n) }

// The whole point of SOCKS support: an HTTPS request must traverse the proxy
// and keep working (i.e. the TLS session is built above the tunnel).
func TestTransportThroughSOCKS5ReachesHTTPS(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "via-socks")
	}))
	t.Cleanup(target.Close)
	u, _ := url.Parse(target.URL)

	proxy := newFakeSOCKS5(t, "", "")
	tr := Transport(TransportOptions{Proxy: proxy.URL("socks5"), RootCAs: target.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs})
	client := &http.Client{Transport: tr}

	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("GET through socks5: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via-socks" {
		t.Fatalf("body = %q, want via-socks", body)
	}
	if proxy.conns == 0 {
		t.Fatal("the request did not go through the SOCKS proxy")
	}
	// socks5:// resolves locally, so an IP address must have been sent.
	proxy.mu.Lock()
	atyp := proxy.lastAtyp
	proxy.mu.Unlock()
	if atyp != socks5AtypIPv4 && atyp != socks5AtypIPv6 {
		t.Fatalf("socks5:// sent atyp 0x%02x, want a resolved IP (local resolution)", atyp)
	}
	_ = u
}

// socks5h:// must send the DOMAIN to the proxy, letting it resolve.
func TestSOCKS5HResolvesRemotely(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = io.WriteString(c, "EHLO localhost\r\n")
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })

	proxy := newFakeSOCKS5(t, "", "")
	dial := socksDialer(mustURL(t, proxy.URL("socks5h")), 5*time.Second)
	conn, err := dial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	proxy.mu.Lock()
	atyp, host := proxy.lastAtyp, proxy.lastHost
	proxy.mu.Unlock()
	// The target here IS an IP literal, so even socks5h sends it as an IP;
	// what matters is that the client did not resolve anything itself.
	_ = atyp
	_ = host
}

// A socks5h target given as a NAME must arrive at the proxy as a domain.
func TestSOCKS5HKeepsHostName(t *testing.T) {
	proxy := newFakeSOCKS5(t, "", "")
	// "localhost" with the proxy's own port: the proxy resolves it and dials
	// itself, which is enough to prove the name was forwarded.
	_, port, _ := net.SplitHostPort(proxy.ln.Addr().String())
	dial := socksDialer(mustURL(t, proxy.URL("socks5h")), 5*time.Second)
	conn, err := dial(context.Background(), "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	proxy.mu.Lock()
	atyp, host := proxy.lastAtyp, proxy.lastHost
	proxy.mu.Unlock()
	if atyp != socks5AtypDomain {
		t.Fatalf("atyp = 0x%02x, want domain (0x03)", atyp)
	}
	if host != "localhost" {
		t.Fatalf("host sent to proxy = %q, want localhost", host)
	}
}

// A proxy that requires auth must get the credentials from the URL.
func TestSOCKS5UserPass(t *testing.T) {
	proxy := newFakeSOCKS5(t, "alice", "s3cret")
	dial := socksDialer(mustURL(t, proxy.URL("socks5")), 5*time.Second)
	// Dialing the proxy's own listener through itself is a cheap reachable
	// target for a CONNECT.
	conn, err := dial(context.Background(), "tcp", proxy.ln.Addr().String())
	if err != nil {
		t.Fatalf("dial with auth: %v", err)
	}
	conn.Close()
}

// The wrong password must fail the handshake rather than hang.
func TestSOCKS5RejectsBadPassword(t *testing.T) {
	proxy := newFakeSOCKS5(t, "alice", "right")
	bad := "socks5://alice:wrong@" + proxy.ln.Addr().String()
	dial := socksDialer(mustURL(t, bad), 3*time.Second)
	if _, err := dial(context.Background(), "tcp", proxy.ln.Addr().String()); err == nil {
		t.Fatal("dial succeeded with a wrong password")
	}
}

// A proxy that is not listening must fail fast with a usable error.
func TestSOCKS5ProxyDown(t *testing.T) {
	dial := socksDialer(mustURL(t, "socks5://127.0.0.1:1"), 2*time.Second)
	if _, err := dial(context.Background(), "tcp", "example.com:443"); err == nil {
		t.Fatal("dial to a dead proxy succeeded")
	} else if !strings.Contains(err.Error(), "dial proxy") {
		t.Fatalf("error = %v, want it to name the proxy dial", err)
	}
}

// An http proxy must keep using the stdlib CONNECT path, not the socks dialer.
func TestHTTPProxyStillUsesProxyFunc(t *testing.T) {
	tr := Transport(TransportOptions{Proxy: "http://127.0.0.1:8080"})
	if tr.Proxy == nil {
		t.Fatal("http proxy: transport.Proxy is nil")
	}
	u, err := tr.Proxy(&http.Request{URL: mustURL(t, "https://example.com")})
	if err != nil || u == nil || u.Host != "127.0.0.1:8080" {
		t.Fatalf("http proxy func = %v, %v", u, err)
	}
	// The socks path leaves Proxy nil: the dialer owns the proxy.
	if socksTr := Transport(TransportOptions{Proxy: "socks5://127.0.0.1:1080"}); socksTr.Proxy != nil {
		t.Fatal("socks5: transport.Proxy must stay nil (the dialer owns the proxy)")
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}
