package fp

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// testCert mints a self-signed certificate for localhost/127.0.0.1 and
// returns it together with a pool that trusts it.
func testCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("load key pair: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("append cert to pool")
	}
	return cert, pool
}

func TestSpecForcesHTTP11(t *testing.T) {
	t.Parallel()
	spec, err := Spec()
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}
	var found bool
	for _, ext := range spec.Extensions {
		alpn, ok := ext.(*utlsALPN)
		if !ok {
			continue
		}
		found = true
		if len(alpn.AlpnProtocols) != 1 || alpn.AlpnProtocols[0] != "http/1.1" {
			t.Fatalf("ALPN = %v, want [http/1.1]", alpn.AlpnProtocols)
		}
	}
	if !found {
		t.Fatal("no ALPN extension in chrome spec")
	}
}

func TestForceALPNAddsExtensionWhenMissing(t *testing.T) {
	t.Parallel()
	spec, err := Spec()
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}
	spec.Extensions = nil
	ForceALPNH11(spec)
	if len(spec.Extensions) != 1 {
		t.Fatalf("extensions = %d, want 1", len(spec.Extensions))
	}
	alpn, ok := spec.Extensions[0].(*utlsALPN)
	if !ok || len(alpn.AlpnProtocols) != 1 || alpn.AlpnProtocols[0] != "http/1.1" {
		t.Fatalf("extension = %#v, want http/1.1 ALPN", spec.Extensions[0])
	}
}

func TestParseProxy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    string
		wantTLS bool
		wantErr bool
	}{
		{"", "", false, false},
		{"127.0.0.1:18080", "127.0.0.1:18080", false, false},
		{"http://proxy:3128", "proxy:3128", false, false},
		{"https://proxy:443", "proxy:443", true, false},
		{"socks5://proxy:1080", "", false, true},
		{"http://", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			p, err := parseProxy(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProxy(%q): %v", tc.in, err)
			}
			if tc.want == "" {
				if p != nil {
					t.Fatalf("proxy = %+v, want nil", p)
				}
				return
			}
			if p == nil || p.host != tc.want || p.tlsProxy != tc.wantTLS {
				t.Fatalf("proxy = %+v, want host %q tls %v", p, tc.want, tc.wantTLS)
			}
		})
	}
}

func TestDialerHandshakeAgainstLocalTLS(t *testing.T) {
	t.Parallel()
	cert, roots := testCert(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "proto=%s alpn=%s", r.Proto, r.TLS.NegotiatedProtocol)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go srv.Serve(ln)
	defer srv.Close()

	dial, err := Dialer(Options{RootCAs: roots})
	if err != nil {
		t.Fatalf("Dialer: %v", err)
	}
	tr := &http.Transport{
		DialTLSContext:    dial,
		ForceAttemptHTTP2: false,
		DisableKeepAlives: true,
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	resp, err := client.Get("https://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	want := "proto=HTTP/1.1 alpn=http/1.1"
	if got := strings.TrimSpace(string(body)); got != want {
		t.Fatalf("server saw %q, want %q", got, want)
	}
}

func TestDialerThroughConnectProxy(t *testing.T) {
	t.Parallel()
	cert, roots := testCert(t)
	target, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer target.Close()
	targetSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "via-proxy")
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go targetSrv.Serve(target)
	defer targetSrv.Close()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	defer proxyLn.Close()
	var connects int
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
				for {
					h, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimSpace(h) == "" {
						break
					}
				}
				dst := strings.Fields(line)[1]
				upstream, err := net.Dial("tcp", dst)
				if err != nil {
					fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer upstream.Close()
				connects++
				fmt.Fprintf(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				done := make(chan struct{})
				go func() {
					io.Copy(upstream, br)
					close(done)
				}()
				io.Copy(c, upstream)
				<-done
			}(conn)
		}
	}()

	dial, err := Dialer(Options{Proxy: "http://" + proxyLn.Addr().String(), RootCAs: roots})
	if err != nil {
		t.Fatalf("Dialer: %v", err)
	}
	client := &http.Client{
		Transport: &http.Transport{DialTLSContext: dial, ForceAttemptHTTP2: false, DisableKeepAlives: true},
		Timeout:   10 * time.Second,
	}
	resp, err := client.Get("https://" + target.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimSpace(string(body)); got != "via-proxy" {
		t.Fatalf("body = %q, want via-proxy", got)
	}
	if connects != 1 {
		t.Fatalf("CONNECT count = %d, want 1", connects)
	}
}

func TestDialerConnectFailure(t *testing.T) {
	t.Parallel()
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer proxyLn.Close()
	go func() {
		conn, err := proxyLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		bufio.NewReader(conn).ReadString('\n')
		fmt.Fprintf(conn, "HTTP/1.1 403 Forbidden\r\n\r\n")
	}()

	dial, err := Dialer(Options{Proxy: proxyLn.Addr().String()})
	if err != nil {
		t.Fatalf("Dialer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = dial(ctx, "tcp", "example.com:443")
	if err == nil || !strings.Contains(err.Error(), "CONNECT") {
		t.Fatalf("err = %v, want CONNECT failure", err)
	}
}

func TestDialContextCancelled(t *testing.T) {
	t.Parallel()
	dial, err := Dialer(Options{})
	if err != nil {
		t.Fatalf("Dialer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dial(ctx, "tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

type utlsALPN = utls.ALPNExtension
