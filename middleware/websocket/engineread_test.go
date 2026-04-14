package websocket

import (
	"errors"
	"io"
	"sync/atomic"
	"testing"
)

// TestChanReaderBasicReadWrite verifies the chanReader yields appended
// chunks via io.Reader semantics with partial-chunk tracking.
func TestChanReaderBasicReadWrite(t *testing.T) {
	r := newChanReader(8, 0, 0)
	r.Append([]byte("hello "))
	r.Append([]byte("world"))

	buf := make([]byte, 4)
	n, err := r.Read(buf)
	if err != nil || n != 4 || string(buf[:n]) != "hell" {
		t.Fatalf("read1: n=%d err=%v buf=%q", n, err, buf[:n])
	}
	n, err = r.Read(buf)
	if err != nil || n != 2 || string(buf[:n]) != "o " {
		t.Fatalf("read2: n=%d err=%v buf=%q", n, err, buf[:n])
	}
	n, _ = r.Read(buf)
	if string(buf[:n]) != "worl" {
		t.Fatalf("read3: %q", buf[:n])
	}
}

// TestChanReaderCloseEOF verifies that closing without an error returns
// io.EOF on subsequent reads.
func TestChanReaderCloseEOF(t *testing.T) {
	r := newChanReader(4, 0, 0)
	r.closeWith(nil)

	_, err := r.Read(make([]byte, 4))
	if err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

// TestChanReaderCloseWithError verifies that closeWith preserves a custom
// error and returns it on the next read.
func TestChanReaderCloseWithError(t *testing.T) {
	r := newChanReader(4, 0, 0)
	want := errors.New("synthetic write error")
	r.closeWith(want)

	_, err := r.Read(make([]byte, 4))
	if err != want {
		t.Errorf("expected %v, got %v", want, err)
	}
}

// TestChanReaderPauseResumeWatermarks verifies the chanReader fires the
// pause callback exactly once when crossing the high-water mark and the
// resume callback exactly once when crossing the low-water mark.
func TestChanReaderPauseResumeWatermarks(t *testing.T) {
	const chanCap = 8 // high=6, low=2
	var pauses, resumes atomic.Uint32
	r := newChanReader(chanCap, 0, 0)
	r.SetPauser(func() { pauses.Add(1) }, func() { resumes.Add(1) })

	// Fill below high-water — no pause yet.
	for range 5 {
		r.Append([]byte("x"))
	}
	if pauses.Load() != 0 {
		t.Errorf("pause called too early: %d", pauses.Load())
	}

	// Cross the high-water mark.
	r.Append([]byte("x")) // depth = 6
	if pauses.Load() != 1 {
		t.Errorf("expected 1 pause, got %d", pauses.Load())
	}

	// Adding more should not retrigger pause (edge-triggered).
	r.Append([]byte("x")) // depth = 7
	r.Append([]byte("x")) // depth = 8
	if pauses.Load() != 1 {
		t.Errorf("expected 1 pause after multiple appends, got %d", pauses.Load())
	}

	// Drain past low-water, expect exactly one resume.
	buf := make([]byte, 1)
	for range 7 { // depth: 8→1
		_, _ = r.Read(buf)
	}
	if resumes.Load() != 1 {
		t.Errorf("expected 1 resume, got %d", resumes.Load())
	}

	// Final read drains the last chunk; resume must not retrigger.
	_, _ = r.Read(buf)
	if resumes.Load() != 1 {
		t.Errorf("resume must be edge-triggered, got %d", resumes.Load())
	}
}

// TestChanReaderDropsOnOverflow verifies that exceeding the channel
// capacity poisons the reader with ErrReadLimit and increments the
// dropped counter.
func TestChanReaderDropsOnOverflow(t *testing.T) {
	r := newChanReader(2, 0, 0) // tiny channel — no pause callback wired
	r.Append([]byte("a"))
	r.Append([]byte("b"))

	// Third append must overflow because no pause callback is wired.
	if ok := r.Append([]byte("c")); ok {
		t.Error("expected third Append to fail (channel full)")
	}
	if r.Dropped() != 1 {
		t.Errorf("expected 1 drop, got %d", r.Dropped())
	}

	// Subsequent reads should yield ErrReadLimit eventually.
	buf := make([]byte, 4)
	// Drain the two queued chunks.
	_, _ = r.Read(buf)
	_, _ = r.Read(buf)
	_, err := r.Read(buf)
	if err != ErrReadLimit {
		t.Errorf("expected ErrReadLimit after overflow, got %v", err)
	}
}

// TestChanReaderSmallCapacityNoThrash verifies that a very small capacity
// (e.g., 2) does not cause pause/resume to thrash (highWater == lowWater).
func TestChanReaderSmallCapacityNoThrash(t *testing.T) {
	var pauses, resumes atomic.Uint32
	r := newChanReader(2, 0, 0) // highWater=1, lowWater=0 (after floor fix)
	r.SetPauser(func() { pauses.Add(1) }, func() { resumes.Add(1) })

	// Fill to highWater — should pause exactly once.
	r.Append([]byte("a")) // depth=1 → crosses highWater=1
	if p := pauses.Load(); p != 1 {
		t.Errorf("expected 1 pause, got %d", p)
	}

	// Drain — should resume exactly once.
	buf := make([]byte, 1)
	_, _ = r.Read(buf) // depth=0 → crosses lowWater=0
	if res := resumes.Load(); res != 1 {
		t.Errorf("expected 1 resume, got %d", res)
	}

	// Refill and drain again — should get exactly one more pause and resume.
	r.Append([]byte("b"))
	if p := pauses.Load(); p != 2 {
		t.Errorf("expected 2 pauses after refill, got %d", p)
	}
	_, _ = r.Read(buf)
	if res := resumes.Load(); res != 2 {
		t.Errorf("expected 2 resumes after redrain, got %d", res)
	}
}

// TestChanReaderConcurrentCloseRace verifies closeWith is idempotent
// under concurrent callers.
func TestChanReaderConcurrentCloseRace(t *testing.T) {
	r := newChanReader(8, 0, 0)
	done := make(chan struct{})
	for range 4 {
		go func() {
			r.closeWith(io.EOF)
			done <- struct{}{}
		}()
	}
	for range 4 {
		<-done
	}
	if !r.closed.Load() {
		t.Error("reader not closed")
	}
}
