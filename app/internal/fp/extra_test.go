package fp

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestConnectTunnelPreservesReadAhead checks that bytes the proxy sends in
// the same write as the CONNECT response head (an eager TLS server flight)
// survive into the returned connection.
func TestConnectTunnelPreservesReadAhead(t *testing.T) {
	t.Parallel()
	client, proxy := net.Pipe()
	defer client.Close()
	go func() {
		defer proxy.Close()
		br := bufio.NewReader(proxy)
		line, _ := br.ReadString('\n')
		if !strings.HasPrefix(line, "CONNECT target.example:443 ") {
			return
		}
		for {
			h, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimSpace(h) == "" {
				break
			}
		}
		// One write: response head plus the first bytes of the tunnel.
		proxy.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\nWAKEUP"))
		io.Copy(io.Discard, br)
	}()

	conn, err := connectTunnel(context.Background(), client, "target.example:443", "")
	if err != nil {
		t.Fatalf("connectTunnel: %v", err)
	}
	buf := make([]byte, 6)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read tunnelled bytes: %v", err)
	}
	if string(buf) != "WAKEUP" {
		t.Fatalf("read %q, want WAKEUP", buf)
	}
}

func TestTransportValidatesProxy(t *testing.T) {
	t.Parallel()
	if _, err := Transport(Options{Proxy: "socks5://x:1"}); err == nil {
		t.Fatal("expected unsupported scheme error")
	}
	tr, err := Transport(Options{})
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	if tr.ForceAttemptHTTP2 {
		t.Fatal("HTTP/2 must stay disabled for the fingerprint bypass")
	}
	if !tr.DisableKeepAlives {
		t.Fatal("keep-alives must stay disabled: fresh ClientHello per request")
	}
}

func TestDialerTLSVerificationFailure(t *testing.T) {
	t.Parallel()
	cert, _ := testCert(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go c.Close()
		}
	}()
	dial, err := Dialer(Options{RootCAs: x509.NewCertPool()})
	if err != nil {
		t.Fatalf("Dialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = dial(ctx, "tcp", ln.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "tls handshake") {
		t.Fatalf("err = %v, want tls handshake failure", err)
	}
}

func TestProxyHostname(t *testing.T) {
	t.Parallel()
	if got := proxyHostname("proxy.example:3128"); got != "proxy.example" {
		t.Fatalf("proxyHostname = %q", got)
	}
	if got := proxyHostname("bare-host"); got != "bare-host" {
		t.Fatalf("proxyHostname = %q", got)
	}
}

func TestDialProxyDialFailure(t *testing.T) {
	t.Parallel()
	dial, err := Dialer(Options{Proxy: "127.0.0.1:1"})
	if err != nil {
		t.Fatalf("Dialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = dial(ctx, "tcp", "example.com:443")
	if err == nil || !strings.Contains(err.Error(), "dial proxy") {
		t.Fatalf("err = %v, want dial proxy failure", err)
	}
	if !strings.Contains(fmt.Sprint(err), "127.0.0.1:1") {
		t.Fatalf("err = %v should name the proxy", err)
	}
}

func TestConnectTunnelSendsProxyAuth(t *testing.T) {
	t.Parallel()
	client, proxy := net.Pipe()
	defer client.Close()

	gotAuth := make(chan string, 1)
	go func() {
		defer proxy.Close()
		br := bufio.NewReader(proxy)
		line, _ := br.ReadString('\n')
		_ = line
		var auth string
		for {
			h, err := br.ReadString('\n')
			if err != nil {
				gotAuth <- auth
				return
			}
			if strings.HasPrefix(strings.ToLower(h), "proxy-authorization: ") {
				auth = strings.TrimSpace(strings.SplitN(h, ":", 2)[1])
			}
			if strings.TrimSpace(h) == "" {
				break
			}
		}
		proxy.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
		gotAuth <- auth
		io.Copy(io.Discard, br)
	}()

	p, err := parseProxy("http://user:s3cret@proxy.example:3128")
	if err != nil {
		t.Fatalf("parseProxy: %v", err)
	}
	conn, err := connectTunnel(context.Background(), client, "target.example:443", p.auth)
	if err != nil {
		t.Fatalf("connectTunnel: %v", err)
	}
	defer conn.Close()
	if got := <-gotAuth; got != "Basic dXNlcjpzM2NyZXQ=" {
		t.Fatalf("Proxy-Authorization = %q", got)
	}
}

func TestConnectTunnelRejectsOversizedLine(t *testing.T) {
	t.Parallel()
	client, proxy := net.Pipe()
	defer client.Close()
	go func() {
		defer proxy.Close()
		bufio.NewReader(proxy).ReadString('\n')
		proxy.Write([]byte("HTTP/1.1 200 OK\r\nX-Huge: " + strings.Repeat("A", 20<<10) + "\r\n\r\n"))
	}()
	_, err := connectTunnel(context.Background(), client, "target.example:443", "")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v, want oversized-line rejection", err)
	}
}

func TestParseProxyAuthRedactedInErrors(t *testing.T) {
	t.Parallel()
	if _, err := parseProxy("http://user:secret@proxy:1"); err != nil {
		t.Fatalf("parseProxy: %v", err)
	}
	if _, err := parseProxy("socks5://user:secret@proxy:1"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v, must not leak credentials", err)
	}
}
