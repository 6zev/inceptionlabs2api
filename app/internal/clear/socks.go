package clear

// SOCKS5 support for IL2API_PROXY.
//
// The module is deliberately dependency-free, so there is no x/net/proxy here:
// these are the ~200 lines of RFC 1928 / RFC 1929 a SOCKS5 client needs, wired
// into http.Transport.DialContext so the proxy sits UNDER the TLS layer. That
// is the whole point for this gateway: the Vercel clearance cookie is chained
// to the negotiated HTTP/2 connection, so a socks5 proxy must carry a real TLS
// handshake to the upstream, not terminate it itself. It also means
// ForceAttemptHTTP2 keeps working through the tunnel.
//
// socks5:// resolves the target host LOCALLY (like curl's socks5); socks5h://
// sends the host name to the proxy (like curl's socks5h), which is what an
// egress proxy usually wants when local DNS is unreliable or poisoned.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"time"
)

// SOCKS5 protocol constants (RFC 1928, RFC 1929).
const (
	socks5Version      = 0x05
	socks5NoAuth       = 0x00
	socks5UserPass     = 0x02
	socks5NoAcceptable = 0xff
	socks5CmdConnect   = 0x01
	socks5AtypIPv4     = 0x01
	socks5AtypDomain   = 0x03
	socks5AtypIPv6     = 0x04
	socks5RepOK        = 0x00
)

// socksReply is the human meaning of a REP byte, used in errors so an operator
// can tell "proxy refused connect" from "host unreachable".
var socksReply = map[byte]string{
	0x01: "general SOCKS server failure",
	0x02: "connection not allowed by ruleset",
	0x03: "network unreachable",
	0x04: "host unreachable",
	0x05: "connection refused",
	0x06: "TTL expired",
	0x07: "command not supported",
	0x08: "address type not supported",
}

// isSOCKS reports whether a normalised proxy URL uses the SOCKS5 scheme.
func isSOCKS(scheme string) bool {
	return scheme == "socks5" || scheme == "socks5h"
}

// socksDialer returns a DialContext that opens a TCP connection to
// proxyAddr and CONNECTs it to the requested target. remoteResolve reports
// socks5h semantics: the host name is sent to the proxy instead of being
// resolved here.
func socksDialer(proxy *url.URL, dialTimeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if dialTimeout <= 0 {
		dialTimeout = 15 * time.Second
	}
	proxyAddr := proxy.Host
	if proxy.Port() == "" {
		proxyAddr = net.JoinHostPort(proxy.Hostname(), "1080")
	}
	remoteResolve := proxy.Scheme == "socks5h"

	var user, pass string
	if proxy.User != nil {
		user = proxy.User.Username()
		pass, _ = proxy.User.Password()
	}

	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("socks5: dial proxy %s: %w", proxyAddr, err)
		}
		// The handshake must not outlive the caller's deadline; a proxy that
		// accepts the TCP connection and then stalls would otherwise hang the
		// request until the idle watchdog fires.
		deadline, _ := ctx.Deadline()
		if deadline.IsZero() {
			deadline = time.Now().Add(dialTimeout)
		}
		_ = conn.SetDeadline(deadline)

		if err := socksHandshake(conn, addr, remoteResolve, user, pass); err != nil {
			_ = conn.Close()
			return nil, err
		}
		// Clear the handshake deadline: the connection now carries a TLS
		// session whose own timeouts apply.
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

// socksHandshake performs the greeting, optional user/pass auth, and the
// CONNECT request on an established TCP connection to the proxy.
func socksHandshake(conn net.Conn, addr string, remoteResolve bool, user, pass string) error {
	// Greeting: version, methods. OFFER user/pass only when a username was
	// configured, so a no-auth proxy is never asked for a method it may reject.
	methods := []byte{socks5NoAuth}
	if user != "" {
		methods = append(methods, socks5UserPass)
	}
	if _, err := conn.Write(append([]byte{socks5Version, byte(len(methods))}, methods...)); err != nil {
		return fmt.Errorf("socks5: greeting: %w", err)
	}
	var sel [2]byte
	if _, err := io.ReadFull(conn, sel[:]); err != nil {
		return fmt.Errorf("socks5: read method: %w", err)
	}
	if sel[0] != socks5Version {
		return fmt.Errorf("socks5: proxy answered version %d", sel[0])
	}
	switch sel[1] {
	case socks5NoAuth:
	case socks5UserPass:
		if user == "" {
			return errors.New("socks5: proxy requires username/password; set it in the proxy URL")
		}
		if err := socksUserPass(conn, user, pass); err != nil {
			return err
		}
	case socks5NoAcceptable:
		return errors.New("socks5: proxy rejected every offered auth method")
	default:
		return fmt.Errorf("socks5: proxy chose unsupported auth method 0x%02x", sel[1])
	}

	req, err := socksConnectRequest(addr, remoteResolve)
	if err != nil {
		return err
	}
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5: connect request: %w", err)
	}

	// Reply: version, rep, rsv, atyp, then the bound address (variable length).
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return fmt.Errorf("socks5: read reply: %w", err)
	}
	if head[0] != socks5Version {
		return fmt.Errorf("socks5: reply version %d", head[0])
	}
	if head[1] != socks5RepOK {
		msg, ok := socksReply[head[1]]
		if !ok {
			msg = fmt.Sprintf("reply 0x%02x", head[1])
		}
		return fmt.Errorf("socks5: connect %s: %s", addr, msg)
	}
	if err := discardSocksAddr(conn, head[3]); err != nil {
		return err
	}
	return nil
}

// socksUserPass sends the RFC 1929 user/pass subnegotiation and checks the
// status byte.
func socksUserPass(conn net.Conn, user, pass string) error {
	if len(user) > 255 || len(pass) > 255 {
		return errors.New("socks5: username or password longer than 255 bytes")
	}
	buf := make([]byte, 0, 3+len(user)+len(pass))
	buf = append(buf, 0x01, byte(len(user)))
	buf = append(buf, user...)
	buf = append(buf, byte(len(pass)))
	buf = append(buf, pass...)
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("socks5: auth: %w", err)
	}
	var st [2]byte
	if _, err := io.ReadFull(conn, st[:]); err != nil {
		return fmt.Errorf("socks5: read auth status: %w", err)
	}
	if st[1] != 0x00 {
		return errors.New("socks5: proxy rejected username/password")
	}
	return nil
}

// socksConnectRequest builds the CONNECT request. With remoteResolve the host
// is sent as a domain name; otherwise it is resolved here and sent as IPv4/IPv6,
// which is what socks5:// means.
func socksConnectRequest(addr string, remoteResolve bool) ([]byte, error) {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("socks5: target %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("socks5: target port %q is not a number", portStr)
	}

	req := []byte{socks5Version, socks5CmdConnect, 0x00}

	ip := net.ParseIP(host)
	switch {
	case remoteResolve && ip == nil:
		if len(host) > 255 {
			return nil, fmt.Errorf("socks5: host name longer than 255 bytes")
		}
		req = append(req, socks5AtypDomain, byte(len(host)))
		req = append(req, host...)
	case ip != nil && ip.To4() != nil:
		req = append(req, socks5AtypIPv4)
		req = append(req, ip.To4()...)
	case ip != nil:
		req = append(req, socks5AtypIPv6)
		req = append(req, ip.To16()...)
	default:
		// Local resolution: socks5:// with a host name.
		ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("socks5: resolve %q: %w", host, err)
		}
		picked := ips[0].IP
		for _, cand := range ips {
			if cand.IP.To4() != nil {
				picked = cand.IP
				break
			}
		}
		if v4 := picked.To4(); v4 != nil {
			req = append(req, socks5AtypIPv4)
			req = append(req, v4...)
		} else {
			req = append(req, socks5AtypIPv6)
			req = append(req, picked.To16()...)
		}
	}
	var portBuf [2]byte
	binary.BigEndian.PutUint16(portBuf[:], uint16(port))
	return append(req, portBuf[:]...), nil
}

// discardSocksAddr reads and drops the BND.ADDR/BND.PORT tail of a reply, so
// the connection is positioned exactly at the start of the tunnel payload.
func discardSocksAddr(conn net.Conn, atyp byte) error {
	var n int
	switch atyp {
	case socks5AtypIPv4:
		n = 4
	case socks5AtypIPv6:
		n = 16
	case socks5AtypDomain:
		var l [1]byte
		if _, err := io.ReadFull(conn, l[:]); err != nil {
			return fmt.Errorf("socks5: reply address: %w", err)
		}
		n = int(l[0])
	default:
		return fmt.Errorf("socks5: reply has unsupported address type 0x%02x", atyp)
	}
	// n address bytes plus the 2-byte port.
	if _, err := io.CopyN(io.Discard, conn, int64(n+2)); err != nil {
		return fmt.Errorf("socks5: reply address: %w", err)
	}
	return nil
}
