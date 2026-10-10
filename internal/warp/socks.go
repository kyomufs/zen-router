package warp

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// SOCKS5 CONNECT transport for the lane (RFC 1928, no-auth only — warp-cli's
// local proxy has no credentials). Hand-rolled to keep zen-router
// dependency-free: the handshake is greeting + CONNECT reply, ~60 lines.

// socksDialer returns a dialer that reaches addr through the local warp-cli
// SOCKS5 proxy. The target is always passed as a DOMAIN (ATYP 0x03) when it
// is a hostname, so DNS resolution happens inside the WARP tunnel — the
// point of the lane is that the egress IP, not just the TCP path, moves.
func (m *Manager) socksDialer() func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, fmt.Errorf("warp: unsupported network %q", network)
		}
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", m.socks)
		if err != nil {
			return nil, fmt.Errorf("warp: dial socks %s: %w", m.socks, err)
		}
		// Bound the handshake by the caller's context (the daemon's
		// attempt budget), then clear the deadline for the tunnel itself.
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		} else {
			_ = conn.SetDeadline(time.Now().Add(cliTimeout))
		}
		if err := m.socksConnect(conn, addr); err != nil {
			_ = conn.Close()
			return nil, err
		}
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

// socksConnect performs the greeting and CONNECT exchange on conn.
func (m *Manager) socksConnect(conn net.Conn, addr string) error {
	// Greeting: VER=5, NMETHODS=1, NO-AUTH.
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return fmt.Errorf("warp: socks greeting: %w", err)
	}
	reply := make([]byte, 2)
	if _, err := readFull(conn, reply); err != nil {
		return fmt.Errorf("warp: socks greeting reply: %w", err)
	}
	if reply[0] != 0x05 || reply[1] != 0x00 {
		return fmt.Errorf("warp: socks greeting refused (ver=%d method=%d)", reply[0], reply[1])
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("warp: split target %q: %w", addr, err)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return fmt.Errorf("warp: bad target port %q: %w", portStr, err)
	}

	// Request: VER CMD=CONNECT RSV ATYP ADDR PORT.
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("warp: target host too long (%d)", len(host))
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	var pb [2]byte
	binary.BigEndian.PutUint16(pb[:], uint16(port))
	req = append(req, pb[:]...)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("warp: socks connect: %w", err)
	}

	// Reply: VER REP RSV ATYP + BND.ADDR + BND.PORT (length follows ATYP).
	head := make([]byte, 4)
	if _, err := readFull(conn, head); err != nil {
		return fmt.Errorf("warp: socks connect reply: %w", err)
	}
	if head[1] != 0x00 {
		return fmt.Errorf("warp: socks connect refused (rep=0x%02x)", head[1])
	}
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		l := make([]byte, 1)
		if _, err := readFull(conn, l); err != nil {
			return fmt.Errorf("warp: socks reply addr: %w", err)
		}
		skip = int(l[0])
	default:
		return fmt.Errorf("warp: socks reply atyp=0x%02x", head[3])
	}
	if _, err := readFull(conn, make([]byte, skip+2)); err != nil {
		return fmt.Errorf("warp: socks reply addr: %w", err)
	}
	return nil
}

func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// Transport builds (once) and returns the lane's HTTP transport: a warm
// connection pool through the SOCKS5 proxy (parity with
// proxy.defaultTransport's pooling — the plugin's client drops keep-alive,
// so the warm pool matters here too). The transport accounts flows for the
// rotation flow-lock. Nil manager = the plain default transport.
func (m *Manager) Transport() http.RoundTripper {
	if m == nil {
		return http.DefaultTransport
	}
	m.transportOnce.Do(func() {
		t := &http.Transport{
			DialContext:           m.socksDialer(),
			ForceAttemptHTTP2:     false, // HTTP/2 over warp adds head-of-line risk for no gain here
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
		m.transport = &flowTransport{inner: t, m: m}
	})
	return m.transport
}

// flowTransport wraps a RoundTripper with the lane's flow-lock counters.
type flowTransport struct {
	inner http.RoundTripper
	m     *Manager
}

func (f *flowTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.m.BeginFlow()
	defer f.m.EndFlow()
	return f.inner.RoundTrip(req)
}
