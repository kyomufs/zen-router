package router

// Dashboard getter surface for plan Task 1 (review F2): the rotating flag
// must be observable as TRUE while a rotation is in flight and must clear,
// stamping LastRotate, when the rotate()/RotateNow deferred body runs.
// Driving rotate() itself would call ensureWarp(forceNew), which registers a
// live device against Cloudflare — finishRotation() is the codebase's
// documented seam for those defers (its doc comment and the stage tests both
// use it), so the in-flight state is seeded exactly the way rotate() sets it.

import (
	"testing"
	"time"
)

func TestRotatingGetterTracksInFlightRotation(t *testing.T) {
	tr := newTestRouter(t, Options{RotationCooldown: 30 * time.Second})

	// rotate() sets rotating=true under mu before the rotation body runs.
	tr.mu.Lock()
	tr.rotating = true
	tr.mu.Unlock()
	if !tr.Rotating() {
		t.Fatal("Rotating() = false during an in-flight rotation, want true")
	}

	// The deferred critical section releases the guard and stamps lastRotate
	// (seam: the rotate()/RotateNow defer body).
	tr.finishRotation()
	if tr.Rotating() {
		t.Error("Rotating() = true after finishRotation, want false")
	}
	last := tr.LastRotate()
	if last.IsZero() {
		t.Fatal("LastRotate() zero after finishRotation, want a stamp")
	}
	// Follow-on for status.last_rotate: the stamp must round-trip RFC3339.
	if _, err := time.Parse(time.RFC3339, last.Format(time.RFC3339)); err != nil {
		t.Errorf("LastRotate %v does not round-trip RFC3339: %v", last, err)
	}
}
