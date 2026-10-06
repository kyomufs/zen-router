package zen

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

// gatedReader blocks in Read until unblock is closed; once released it serves
// data and then io.EOF. Only the watchdog pump goroutine touches its fields.
type gatedReader struct {
	unblock chan struct{}
	data    []byte
}

func (g *gatedReader) Read(p []byte) (int, error) {
	<-g.unblock
	if len(g.data) > 0 {
		n := copy(p, g.data)
		g.data = g.data[n:]
		return n, nil
	}
	return 0, io.EOF
}

// byteThenGatedReader serves one byte on the first Read, then blocks until
// unblock is closed and finally returns io.EOF.
type byteThenGatedReader struct {
	unblock chan struct{}
	sent    bool
}

func (r *byteThenGatedReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		p[0] = 'x'
		return 1, nil
	}
	<-r.unblock
	return 0, io.EOF
}

// chunkedReader serves data in chunks of at most per bytes (0 = unlimited),
// optionally sleeping delay before each Read, then io.EOF.
type chunkedReader struct {
	data  []byte
	per   int
	delay time.Duration
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	n := len(c.data)
	if n > len(p) {
		n = len(p)
	}
	if c.per > 0 && n > c.per {
		n = c.per
	}
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

// slowStartReader sleeps delay in its first Read, serves one byte, then EOF.
type slowStartReader struct {
	delay    time.Duration
	returned bool
}

func (s *slowStartReader) Read(p []byte) (int, error) {
	if !s.returned {
		time.Sleep(s.delay)
		s.returned = true
		p[0] = 'x'
		return 1, nil
	}
	return 0, io.EOF
}

func TestWatchdogFirstEventTimeout(t *testing.T) {
	r := &gatedReader{unblock: make(chan struct{})}
	defer close(r.unblock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wd := NewWatchdog(ctx, r, 20*time.Millisecond, 20*time.Millisecond)
	buf := make([]byte, 8)

	start := time.Now()
	n, err := wd.Read(buf)
	elapsed := time.Since(start)

	if n != 0 {
		t.Fatalf("Read returned %d bytes on timeout, want 0", n)
	}
	if !errors.Is(err, ErrWatchdogTimeout) {
		t.Fatalf("Read error = %v, want errors.Is(err, ErrWatchdogTimeout)", err)
	}
	if elapsed < 10*time.Millisecond {
		t.Fatalf("first-event timeout fired after %v, want ~20ms", elapsed)
	}
	if elapsed >= time.Second {
		t.Fatalf("first Read took %v, want well under 1s", elapsed)
	}
}

func TestWatchdogIdleTimeout(t *testing.T) {
	r := &byteThenGatedReader{unblock: make(chan struct{})}
	defer close(r.unblock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wd := NewWatchdog(ctx, r, 20*time.Millisecond, 20*time.Millisecond)
	buf := make([]byte, 8)

	n, err := wd.Read(buf)
	if err != nil || n != 1 {
		t.Fatalf("first Read = (%d, %v), want (1, nil)", n, err)
	}

	start := time.Now()
	n, err = wd.Read(buf)
	elapsed := time.Since(start)

	if n != 0 {
		t.Fatalf("Read returned %d bytes on timeout, want 0", n)
	}
	if !errors.Is(err, ErrWatchdogTimeout) {
		t.Fatalf("idle Read error = %v, want errors.Is(err, ErrWatchdogTimeout)", err)
	}
	if elapsed < 10*time.Millisecond {
		t.Fatalf("idle timeout fired after %v, want ~20ms", elapsed)
	}
	if elapsed >= time.Second {
		t.Fatalf("idle Read took %v, want well under 1s", elapsed)
	}
}

func TestWatchdogDisarms(t *testing.T) {
	want := "hello world, EOF ahead"
	r := &chunkedReader{data: []byte(want), per: 4}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Tiny budget: EOF must disarm the watchdog instead of timing out.
	wd := NewWatchdog(ctx, r, 20*time.Millisecond, 20*time.Millisecond)
	buf := make([]byte, 8)

	var got []byte
	var err error
	for i := 0; i < 100; i++ {
		var n int
		n, err = wd.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if err == nil {
		t.Fatal("stream never ended")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("final Read error = %v, want io.EOF", err)
	}
	if err != io.EOF {
		t.Fatalf("io.EOF not propagated untouched: %v", err)
	}
	if string(got) != want {
		t.Fatalf("read %q, want %q", got, want)
	}
}

func TestWatchdogContextCancel(t *testing.T) {
	r := &gatedReader{unblock: make(chan struct{})}
	defer close(r.unblock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Budgets far above the cancel point so a timeout can never win.
	wd := NewWatchdog(ctx, r, 200*time.Millisecond, 200*time.Millisecond)
	done := make(chan error, 1)
	go func() {
		_, err := wd.Read(make([]byte, 8))
		done <- err
	}()

	time.Sleep(5 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Read did not return promptly after context cancel")
	}
}

func TestWatchdogDisabled(t *testing.T) {
	r := &slowStartReader{delay: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Budgets <= 0 disable the watchdog entirely.
	wd := NewWatchdog(ctx, r, 0, 0)
	buf := make([]byte, 8)

	start := time.Now()
	n, err := wd.Read(buf)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Read with disabled budgets = (%d, %v), want (1, nil)", n, err)
	}
	if n != 1 || buf[0] != 'x' {
		t.Fatalf("Read = (%d, %q), want (1, 'x')", n, buf[:n])
	}
	if elapsed < 45*time.Millisecond {
		t.Fatalf("Read returned after %v, want it to wait ~50ms for data", elapsed)
	}
	if elapsed >= time.Second {
		t.Fatalf("Read took %v, want well under 1s", elapsed)
	}

	_, err = wd.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("second Read = %v, want io.EOF", err)
	}
}

func TestWatchdogSteadyStream(t *testing.T) {
	data := make([]byte, 20)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	r := &chunkedReader{data: data, per: 1, delay: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wd := NewWatchdog(ctx, r, 50*time.Millisecond, 50*time.Millisecond)
	buf := make([]byte, 4)

	var got []byte
	for i := 0; i < 20; i++ {
		n, err := wd.Read(buf)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if errors.Is(err, ErrWatchdogTimeout) {
			t.Fatalf("read %d timed out on a steady stream", i)
		}
		got = append(got, buf[:n]...)
	}
	if len(got) != len(data) {
		t.Fatalf("read %d bytes, want %d", len(got), len(data))
	}

	_, err := wd.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("final Read = %v, want io.EOF", err)
	}
}
