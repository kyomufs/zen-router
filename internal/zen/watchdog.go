// Stream watchdog reader — see NewWatchdog for the full design note
// (byte-level first-event/idle phase mapping, pump goroutine trade-off).
//
// This file must not become the package doc comment: models.go owns it.
package zen

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ErrWatchdogTimeout is the sentinel wrapped by errors returned when a Read
// exceeds the active budget (first-event or body-idle). Comparable with
// errors.Is.
var ErrWatchdogTimeout = errors.New("zen: stream watchdog timeout")

// readResult is one completed Read from the underlying reader, carried to
// the caller goroutine over watchdog.ch.
type readResult struct {
	b   []byte // copy of the bytes read (len == n)
	err error  // error returned by the underlying Read, if any
}

// watchdog implements io.Reader over r with first-event / body-idle budgets.
// Read is not safe for concurrent use by multiple goroutines (the standard
// io.Reader contract); r itself is only ever touched by the pump goroutine.
type watchdog struct {
	ctx        context.Context
	r          io.Reader
	firstEvent time.Duration
	idle       time.Duration

	start sync.Once       // lazily launches pump on first Read
	ch    chan readResult // buffered (cap 1): pump hands off results

	// Read-side state, touched only by the caller goroutine:
	sawByte bool   // first byte delivered to the caller -> idle phase
	stash   []byte // unconsumed tail of an oversized pump chunk
	final   error  // terminal error from r, sticky once delivered
}

// NewWatchdog wraps r so that every Read completes within firstEvent until
// the first byte is delivered to the caller and within idle afterwards;
// exceeding the budget yields an error satisfying errors.Is(err,
// ErrWatchdogTimeout). io.EOF and any other error from r propagate untouched
// (EOF disarms the watchdog). Read returns ctx.Err() promptly when ctx is
// cancelled. A budget <= 0 disables that phase's timeout. ctx must be
// non-nil.
//
// Phase mapping vs the reference implementation: the plugin createWatchdog
// (dsh-opencode-zen lib/index.js) arms FIRST_EVENT until the first *parsed
// SSE data line* and BODY_IDLE after that, re-arming a timer around every
// reader.read(). This wrapper is a plain io.Reader and knows nothing about
// SSE lines, so the mapping is byte-level: the first-event budget covers
// every Read until the first byte reaches the caller; from then on each Read
// is governed by the idle budget. The line-level semantics ("first event" =
// first data line, not first byte) belong to the SSE layer above
// (internal/gateway, Task 12), which wires the consumer defaults: 30s
// first-event, 120s chat body-idle, 300s responses body-idle.
//
// Design: persistent pump goroutine, not a per-Read timer racing r.Read.
// A per-Read timer cannot cancel the blocked underlying Read: after a
// timeout, a subsequent Read would either start a second concurrent Read on
// r (io.Reader is not safe for concurrent use) or have to be refused
// outright. Instead a single pump goroutine — started lazily on the first
// Read — is the sole reader of r; every caller Read waits on the pump's
// result channel with a timer racing ctx.Done(). Trade-off: the pump
// consumes bytes from r even while the caller is not reading (up to one
// chunk buffered ahead), which for a streaming SSE body is desirable: it
// decouples upstream pacing from consumer pacing. Caveat: a pump goroutine
// blocked inside r.Read cannot be interrupted; it exits once r unblocks (its
// result is dropped via the ctx.Done() arm of the send select). Callers that
// receive ErrWatchdogTimeout are expected to cancel ctx and close the
// underlying stream — which is how the gateway wires this reader.
func NewWatchdog(ctx context.Context, r io.Reader, firstEvent, idle time.Duration) io.Reader {
	return &watchdog{
		ctx:        ctx,
		r:          r,
		firstEvent: firstEvent,
		idle:       idle,
		ch:         make(chan readResult, 1),
	}
}

// pump is the sole reader of r. It runs until r returns an error (the final
// result is handed off, then it exits) or ctx is cancelled.
func (w *watchdog) pump() {
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}
		n, err := w.r.Read(buf)
		res := readResult{err: err}
		if n > 0 {
			// Copy: the buffer is reused, but a result may sit in the
			// channel while the pump reads again.
			res.b = make([]byte, n)
			copy(res.b, buf[:n])
		}
		select {
		case w.ch <- res:
		case <-w.ctx.Done():
			// Abandoned: drop the result instead of blocking forever.
			return
		}
		if err != nil {
			return
		}
	}
}

func (w *watchdog) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Launch the pump asynchronously: sync.Once.Do blocks until f returns,
	// and the pump only returns when the stream ends or ctx is cancelled.
	w.start.Do(func() { go w.pump() })

	// Arm one timer for the whole Read; defer guarantees it is stopped.
	budget := w.firstEvent
	phase := "first-event"
	if w.sawByte {
		budget = w.idle
		phase = "body-idle"
	}
	var timerC <-chan time.Time
	if budget > 0 {
		timer := time.NewTimer(budget)
		defer timer.Stop()
		timerC = timer.C
	}

	for {
		// Bytes left over from an oversized chunk: deliver instantly.
		if len(w.stash) > 0 {
			n := copy(p, w.stash)
			w.stash = w.stash[n:]
			if n > 0 {
				w.sawByte = true
			}
			return n, nil
		}
		// Terminal error from r, sticky per the io.Reader contract.
		if w.final != nil {
			return 0, w.final
		}
		select {
		case <-w.ctx.Done():
			return 0, w.ctx.Err()
		default:
		}

		select {
		case res := <-w.ch:
			if len(res.b) > 0 {
				n := copy(p, res.b)
				if n < len(res.b) {
					w.stash = res.b[n:]
				}
				w.sawByte = true
				if res.err != nil {
					// Keep the terminal error until the bytes drain.
					w.final = res.err
				}
				return n, nil
			}
			if res.err != nil {
				// Propagate untouched (io.EOF disarms the watchdog).
				w.final = res.err
				return 0, res.err
			}
			// (0, nil): discouraged by io.Reader; wait for the next result.
		case <-w.ctx.Done():
			return 0, w.ctx.Err()
		case <-timerC:
			return 0, fmt.Errorf("zen: %s watchdog timeout after %s: %w", phase, budget, ErrWatchdogTimeout)
		}
	}
}
