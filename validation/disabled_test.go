//go:build !validation

package validation

import (
	"testing"
	"unsafe"
)

// TestProductionCounterIsZeroSize asserts the production Counter type
// occupies zero bytes — Snapshot embeds Counters unconditionally and
// observe.Snapshot embeds it too, so any accidental size leak shows up
// here before it can pessimize the production hot path.
func TestProductionCounterIsZeroSize(t *testing.T) {
	if got := unsafe.Sizeof(Counter{}); got != 0 {
		t.Fatalf("unsafe.Sizeof(Counter{}) = %d, want 0", got)
	}
}

// TestProductionAddIsNoOp confirms Counter.Add is the inert stub: a
// million increments still observe Load() == 0. If a future refactor
// accidentally points the production Counter at a real atomic, this
// test catches the regression before benchmarks do.
func TestProductionAddIsNoOp(t *testing.T) {
	var c Counter
	for range 1_000_000 {
		c.Add(1)
	}
	if got := c.Load(); got != 0 {
		t.Fatalf("Counter.Load after 1e6 Add(1): got %d, want 0", got)
	}
}

// TestProductionRecordPanicNoOp exercises the helper too — same
// invariant, different surface.
func TestProductionRecordPanicNoOp(t *testing.T) {
	for range 1_000_000 {
		RecordPanic()
	}
	if got := PanicCount.Load(); got != 0 {
		t.Fatalf("PanicCount.Load after 1e6 RecordPanic(): got %d, want 0", got)
	}
}

// TestProductionSnapshotIsZero confirms Snapshot returns the zero
// value in production builds regardless of any "increments" the
// caller attempted.
func TestProductionSnapshotIsZero(t *testing.T) {
	PanicCount.Add(1)
	JWTLateAdmits.Add(99)
	IouringSQECorruptions.Add(7)
	got := Snapshot()
	if got != (Counters{}) {
		t.Fatalf("Snapshot() = %+v, want zero value", got)
	}
}
