package cli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestWedgedDaemonTimeoutIsNotErrNotRunning (review F2): a request killed
// by the caller's context deadline reports a WEDGED daemon, not a dead
// one. The ErrNotRunning hint ("start it with `zen-router up`") would send
// the user to start a daemon that is already running.
//
// Hermetic: httptest on an ephemeral loopback port that never answers —
// no live daemon, no external network.
func TestWedgedDaemonTimeoutIsNotErrNotRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Wedged daemon: never answer; release as soon as the client goes away.
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewControlClient(strings.TrimPrefix(srv.URL, "http://"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.Status(ctx)
	if err == nil {
		t.Fatal("expected an error from a wedged daemon")
	}
	if errors.Is(err, ErrNotRunning) {
		t.Errorf("deadline misreported as ErrNotRunning: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want context.DeadlineExceeded in the error chain, got: %v", err)
	}
	if !strings.Contains(err.Error(), "GET "+"/_zenctl/status") && !strings.Contains(err.Error(), "GET status") {
		t.Errorf("want the failing control request named in the error, got: %v", err)
	}
}

// TestUnreachableDaemonStillReportsNotRunning: connection refused keeps the
// ErrNotRunning mapping — the hint is right when nothing listens at all.
// Hermetic: a just-closed ephemeral port on loopback, no live daemon.
func TestUnreachableDaemonStillReportsNotRunning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	c := NewControlClient(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = c.Status(ctx)
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("want ErrNotRunning for connection refused, got: %v", err)
	}
}
