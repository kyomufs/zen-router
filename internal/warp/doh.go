package warp

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"time"
)

// dohServer is Cloudflare's DNS-over-HTTPS endpoint. It is reached inside the
// WireGuard tunnel so the answer reflects WARP's view of the internet, not the
// fake-IP mapping FlClash injects into the system resolver.
const dohServer = "https://1.1.1.1/dns-query"

// Resolver performs DNS lookups over HTTPS through a caller-supplied
// transport (typically one bound to the WARP device).
type Resolver struct {
	client *http.Client
}

// NewResolver builds a DoH resolver on top of the given transport. Passing a
// nil transport uses the default client (direct egress).
func NewResolver(transport http.RoundTripper) *Resolver {
	client := &http.Client{Timeout: 15 * time.Second}
	if transport != nil {
		client.Transport = transport
	}
	return &Resolver{client: client}
}

// LookupIP resolves host to A/AAAA records via DoH and returns all answers.
func (r *Resolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	names := []uint16{1, 28} // A then AAAA
	var ips []net.IP
	var lastErr error
	for _, qtype := range names {
		msg, err := buildQuery(host, qtype)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, dohServer, bytes.NewReader(msg))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/dns-message")
		req.Header.Set("Accept", "application/dns-message")
		resp, err := r.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("DoH HTTP %d", resp.StatusCode)
			continue
		}
		answers, err := parseAnswers(body)
		if err != nil {
			lastErr = err
			continue
		}
		ips = append(ips, answers...)
	}
	if len(ips) == 0 {
		if lastErr != nil {
			return nil, fmt.Errorf("DoH lookup %s: %w", host, lastErr)
		}
		return nil, fmt.Errorf("DoH lookup %s: no records", host)
	}
	return ips, nil
}

// Dialer returns a dialer that resolves through DoH and dials the resolved
// address, so TCP connections to the target avoid the poisoned system DNS.
func (r *Resolver) Dialer(network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	// If the target is already an IP, skip resolution.
	if ip := net.ParseIP(host); ip != nil {
		return net.Dial(network, address)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ips, err := r.LookupIP(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		conn, err := net.Dial(network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("dial %s after DoH: %w", address, lastErr)
}

// ResolveHostIP resolves a hostname to its first A (or AAAA) address via DoH on
// the direct egress path. This is used for the WireGuard server endpoint: the
// WARP API advertises it as a hostname, but the kernel tunnel needs a concrete
// IP, and the system resolver would return a fake-IP under an active TUN.
func ResolveHostIP(ctx context.Context, host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return host, nil
	}
	r := NewResolver(nil) // direct egress; the tunnel endpoint must not recurse
	ips, err := r.LookupIP(ctx, host)
	if err != nil {
		return "", err
	}
	// Prefer IPv4 — the wg endpoint we configured carries a v4 port list.
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4.String(), nil
		}
	}
	return ips[0].String(), nil
}

// --- DNS wire format (RFC 1035) -------------------------------------------

func buildQuery(name string, qtype uint16) ([]byte, error) {
	var buf bytes.Buffer
	id := uint16(rand.Intn(65535))
	if err := binary.Write(&buf, binary.BigEndian, id); err != nil {
		return nil, err
	}
	// flags: standard query, recursion desired
	if err := binary.Write(&buf, binary.BigEndian, uint16(0x0100)); err != nil {
		return nil, err
	}
	for _, v := range []uint16{1, 0, 1, 0} { // qdcount=1, ancount=0, nscount=0, arcount=0
		if err := binary.Write(&buf, binary.BigEndian, v); err != nil {
			return nil, err
		}
	}
	for _, label := range splitLabels(name) {
		if len(label) > 63 {
			return nil, fmt.Errorf("label too long: %q", label)
		}
		buf.WriteByte(byte(len(label)))
		buf.WriteString(label)
	}
	buf.WriteByte(0)
	if err := binary.Write(&buf, binary.BigEndian, qtype); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, uint16(1)); err != nil { // class IN
		return nil, err
	}
	return buf.Bytes(), nil
}

func splitLabels(name string) []string {
	var labels []string
	start := 0
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			if i > start {
				labels = append(labels, name[start:i])
			}
			start = i + 1
		}
	}
	if start < len(name) {
		labels = append(labels, name[start:])
	}
	return labels
}

// parseAnswers extracts A/AAAA addresses from a DNS response. It walks the
// answer section and decodes resource records by type.
func parseAnswers(msg []byte) ([]net.IP, error) {
	if len(msg) < 12 {
		return nil, fmt.Errorf("DNS response too short")
	}
	qdcount := binary.BigEndian.Uint16(msg[4:6])
	ancount := binary.BigEndian.Uint16(msg[6:8])
	offset := 12

	// Skip questions.
	for i := 0; i < int(qdcount); i++ {
		off, err := skipName(msg, offset)
		if err != nil {
			return nil, err
		}
		offset = off + 4 // type + class
		if offset > len(msg) {
			return nil, fmt.Errorf("truncated question section")
		}
	}

	var ips []net.IP
	for i := 0; i < int(ancount); i++ {
		off, err := skipName(msg, offset)
		if err != nil {
			return nil, err
		}
		offset = off
		if offset+10 > len(msg) {
			return nil, fmt.Errorf("truncated answer record")
		}
		rtype := binary.BigEndian.Uint16(msg[offset : offset+2])
		rdlength := binary.BigEndian.Uint16(msg[offset+8 : offset+10])
		offset += 10
		if offset+int(rdlength) > len(msg) {
			return nil, fmt.Errorf("truncated rdata")
		}
		rdata := msg[offset : offset+int(rdlength)]
		switch rtype {
		case 1: // A
			if len(rdata) == 4 {
				ip := make(net.IP, 4)
				copy(ip, rdata)
				ips = append(ips, ip)
			}
		case 28: // AAAA
			if len(rdata) == 16 {
				ip := make(net.IP, 16)
				copy(ip, rdata)
				ips = append(ips, ip)
			}
		}
		offset += int(rdlength)
	}
	return ips, nil
}

// skipName advances past a (possibly compressed) domain name.
func skipName(msg []byte, offset int) (int, error) {
	for {
		if offset >= len(msg) {
			return 0, fmt.Errorf("name out of bounds")
		}
		l := int(msg[offset])
		if l == 0 {
			return offset + 1, nil
		}
		if l&0xC0 == 0xC0 { // compression pointer
			if offset+1 >= len(msg) {
				return 0, fmt.Errorf("truncated compression pointer")
			}
			return offset + 2, nil
		}
		offset += 1 + l
	}
}
