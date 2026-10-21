package clear

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestTransportNegotiatesHTTP2 proves the property the old design violated:
// the transport advertises h2 and a local h2-capable server sees HTTP/2.
func TestTransportNegotiatesHTTP2(t *testing.T) {
	t.Parallel()
	cert, roots := tlsServerForCert(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2", "http/1.1"},
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	gotProto := make(chan string, 1)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case gotProto <- r.Proto:
			default:
			}
			// Clearance cookie must survive the round trip.
			ck, _ := r.Cookie(CookieName)
			if ck != nil {
				w.Header().Set("x-seen-cookie", ck.Value)
			}
			io.WriteString(w, "ok")
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go srv.Serve(ln)
	defer srv.Close()

	tr := Transport(TransportOptions{RootCAs: roots})
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	// Go's test TLS cert is for 127.0.0.1; RootCAs above trusts our CA.
	req, _ := http.NewRequest(http.MethodGet, "https://"+ln.Addr().String()+"/", nil)
	req.Header.Set("user-agent", UserAgent)
	req.Header.Set("cookie", CookieName+"=abc")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	select {
	case proto := <-gotProto:
		if proto != "HTTP/2.0" {
			t.Fatalf("server saw %s, want HTTP/2.0 (h2 is required by the edge)", proto)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never ran")
	}
	if got := resp.Header.Get("x-seen-cookie"); got != "abc" {
		t.Fatalf("cookie header not delivered: %q", got)
	}
}

func TestTransportUsesProxy(t *testing.T) {
	t.Parallel()
	cert, roots := tlsServerForCert(t)
	_ = cert
	// Target TLS server (any cert is trusted only via the tunnel; the proxy
	// test asserts the CONNECT happened, not that the handshake completes).
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}})
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer ln.Close()

	var connects atomic.Int32
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	defer proxyLn.Close()
	go func() {
		for {
			conn, err := proxyLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				line, err := br.ReadString('\n')
				if err != nil || !strings.HasPrefix(line, "CONNECT ") {
					return
				}
				connects.Add(1)
				for {
					h, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimSpace(h) == "" {
						break
					}
				}
				io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
			}(conn)
		}
	}()

	tr := Transport(TransportOptions{Proxy: "http://" + proxyLn.Addr().String(), RootCAs: roots, DialTimeout: time.Second})
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	_, err = client.Get("https://" + ln.Addr().String() + "/")
	if err == nil {
		t.Fatal("expected the proxy refusal to surface")
	}
	if connects.Load() != 1 {
		t.Fatalf("CONNECT count = %d, want 1", connects.Load())
	}
}

func TestTransportForceAttemptHTTP2(t *testing.T) {
	t.Parallel()
	tr := Transport(TransportOptions{})
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 must be true: h1.1 can never hold clearance")
	}
	if tr.DisableKeepAlives {
		t.Fatal("keep-alives must stay on so the h2 connection is reused")
	}
	if tr.TLSClientConfig != nil {
		t.Fatal("no TLS override without injected roots")
	}
}

func TestTransportInjectedRoots(t *testing.T) {
	t.Parallel()
	roots := x509.NewCertPool()
	tr := Transport(TransportOptions{RootCAs: roots})
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs != roots {
		t.Fatal("RootCAs not applied")
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d", tr.TLSClientConfig.MinVersion)
	}
}

func TestTransportResponseHeaderTimeout(t *testing.T) {
	t.Parallel()
	tr := Transport(TransportOptions{ResponseHeaderTimeout: 3 * time.Second})
	if tr.ResponseHeaderTimeout != 3*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %s", tr.ResponseHeaderTimeout)
	}
}

func TestIsChallenge(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		hdr    map[string]string
		want   bool
	}{
		{"mitigated challenge", 429, map[string]string{"x-vercel-mitigated": "challenge"}, true},
		{"token header", 429, map[string]string{"x-vercel-challenge-token": "t"}, true},
		{"plain 429", 429, nil, false},
		{"403 is not retried", 403, map[string]string{"x-vercel-mitigated": "challenge"}, false},
		{"200", 200, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{}}
			for k, v := range tc.hdr {
				resp.Header.Set(k, v)
			}
			if got := IsChallenge(resp); got != tc.want {
				t.Fatalf("IsChallenge = %v, want %v", got, tc.want)
			}
		})
	}
	if IsChallenge(nil) {
		t.Fatal("nil response must not be a challenge")
	}
}

func TestUserAgentAndCookieName(t *testing.T) {
	t.Parallel()
	if CookieName != "_vcrcs" {
		t.Fatalf("CookieName = %q", CookieName)
	}
	if !strings.Contains(UserAgent, "Chrome/141") {
		t.Fatalf("UserAgent = %q", UserAgent)
	}
}
