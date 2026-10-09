package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Upstream is the real zen gateway the proxy forwards to.
const Upstream = "https://opencode.ai"

// DAILY_LIMIT_RE mirrors the plugin's own daily-quota detector so the proxy
// rotates on exactly the same gateway message the plugin keys off. Sniffing
// is gated on a 429 like golden (lib/index.js:1579 guards the match at
// :1583): router.OnResult treats DailyLimit as quota exhaustion even on a
// 2xx stream, and a success body may legitimately quote a class name (DM-7).
var DAILY_LIMIT_RE = regexp.MustCompile(`FreeUsageLimitError|GoUsageLimitError|BlackUsageLimitError`)

// peekLimit bounds how much of a response body is buffered for quota sniffing.
// A quota rejection arrives as the first (and only) chunk, well under this.
const peekLimit = 8 << 10

// Egress identifies which path a request took, for accounting and rotation.
type Egress string

const (
	EgressDirect Egress = "direct"
)

// peekBody tees the first peekLimit bytes so the proxy can sniff the gateway's
// error type without consuming or re-buffering a streamed body.
type peekBody struct {
	inner io.ReadCloser
	buf   bytes.Buffer
	once  sync.Once
	done  chan struct{}
}

func newPeekBody(inner io.ReadCloser) *peekBody {
	return &peekBody{inner: inner, done: make(chan struct{})}
}

func (p *peekBody) Read(b []byte) (int, error) {
	n, err := p.inner.Read(b)
	if n > 0 && p.buf.Len() < peekLimit {
		room := peekLimit - p.buf.Len()
		if n < room {
			room = n
		}
		p.buf.Write(b[:room])
	}
	return n, err
}

func (p *peekBody) Close() error {
	p.once.Do(func() { close(p.done) })
	return p.inner.Close()
}

func (p *peekBody) peeked() string {
	return p.buf.String()
}

// Result is what the proxy observed for one proxied call.
type Result struct {
	Egress     Egress
	Status     int
	DailyLimit bool
	RetryAfter string
	Peek       string
	LatencyMS  int64
	Err        error
}

// ResultHook receives the observation for every upstream response.
type ResultHook func(Result)

// Config configures the proxy server.
type Config struct {
	Listen      string // e.g. 127.0.0.1:8787
	UpstreamURL string // defaults to Upstream
	OnResult    ResultHook
	Logger      *log.Logger

	// Egress returns the current outbound transport. It is called per request
	// so rotation can swap the path without restarting the server.
	Egress func() (Egress, http.RoundTripper)
}

// Server is the local reverse proxy that fronts the zen gateway.
type Server struct {
	cfg      Config
	upstream *url.URL
	proxy    *httputil.ReverseProxy
	logger   *log.Logger
}

// New builds a Server from cfg.
func New(cfg Config) (*Server, error) {
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8787"
	}
	raw := cfg.UpstreamURL
	if raw == "" {
		raw = Upstream
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse upstream url: %w", err)
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Egress == nil {
		cfg.Egress = func() (Egress, http.RoundTripper) { return EgressDirect, defaultTransport(nil) }
	}
	s := &Server{cfg: cfg, upstream: u, logger: cfg.Logger}
	s.proxy = &httputil.ReverseProxy{
		Director:      s.director,
		Transport:     roundTripperFunc(s.transport),
		FlushInterval: -1, // stream SSE chunks immediately
		ErrorHandler:  s.onError,
	}
	return s, nil
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// director rewrites the inbound request to point at the upstream gateway.
func (s *Server) director(req *http.Request) {
	req.URL.Scheme = s.upstream.Scheme
	req.URL.Host = s.upstream.Host
	req.Host = s.upstream.Host
	// Keep the caller's path (the plugin already targets /zen/v1/...).
	if !strings.HasPrefix(req.URL.Path, "/") {
		req.URL.Path = "/" + req.URL.Path
	}
	// Hop-by-hop headers must not leak upstream.
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authenticate")
	req.Header.Del("Proxy-Authorization")
}

// transport selects the active egress and forwards the request. The response
// body is wrapped here (not in ModifyResponse) so the egress name captured
// from the request context is available when the quota sniff fires.
func (s *Server) transport(req *http.Request) (*http.Response, error) {
	name, rt := s.cfg.Egress()
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	s.wrapBody(req, name, resp)
	return resp, nil
}

// wrapBody tees the response for quota sniffing and wires the result hook.
func (s *Server) wrapBody(req *http.Request, egress Egress, resp *http.Response) {
	started := time.Now()
	pb := newPeekBody(resp.Body)
	resp.Body = &hookBody{ReadCloser: pb, done: func() {
		if s.cfg.OnResult == nil {
			return
		}
		s.cfg.OnResult(Result{
			Egress:     egress,
			Status:     resp.StatusCode,
			DailyLimit: resp.StatusCode == 429 && DAILY_LIMIT_RE.MatchString(pb.peeked()),
			RetryAfter: resp.Header.Get("retry-after"),
			Peek:       strings.TrimSpace(pb.peeked()),
			LatencyMS:  time.Since(started).Milliseconds(),
		})
	}}
}

// hookBody fires a callback once the body has been closed (fully consumed or
// abandoned).
type hookBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (h *hookBody) Close() error {
	h.once.Do(h.done)
	return h.ReadCloser.Close()
}

func (s *Server) onError(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Printf("proxy error %s %s: %v", r.Method, r.URL.Path, err)
	// Surface as a 502 so the plugin classifies it as a transport failure and
	// the host retry policy takes over.
	http.Error(w, `{"error":{"type":"proxy_error","message":"zen-router upstream failure"}}`, http.StatusBadGateway)
}

// Handler exposes the reverse proxy as an http.Handler.
func (s *Server) Handler() http.Handler { return s.proxy }

// ListenAndServe runs the proxy until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Listen, err)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()
	s.logger.Printf("zen-router proxy listening on http://%s -> %s", s.cfg.Listen, s.upstream.String())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// --- transports ------------------------------------------------------------

// defaultTransport builds an outbound transport. This is the latency fix: the
// plugin's undici client drops keep-alive after ~4s, so every agent request
// pays a fresh TCP+TLS handshake through the FlClash tunnel (~500ms). Keeping
// a warm pool here reuses that handshake across requests.
func defaultTransport(dialer *net.Dialer) *http.Transport {
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

// DirectTransport routes through the normal system path (FlClash TUN).
func DirectTransport() http.RoundTripper { return defaultTransport(nil) }
