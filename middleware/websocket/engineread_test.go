package websocket

import (
	"errors"
	"io"
	"runtime"
	"sync"
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

// TestChanReaderSpillsOnOverflow verifies that chunks arriving after the
// channel fills are queued in the bounded spill buffer rather than
// discarded (celeris#484), and that ErrReadLimit is raised only once both
// the channel and the spill are full.
func TestChanReaderSpillsOnOverflow(t *testing.T) {
	r := newChanReader(2, 0, 0) // cap 2, spillMax 2 — no pause callback wired
	for _, c := range []string{"a", "b"} {
		if !r.Append([]byte(c)) {
			t.Fatalf("Append(%q) into empty channel must succeed", c)
		}
	}
	// Channel is full; these must spill, not drop.
	for _, c := range []string{"c", "d"} {
		if !r.Append([]byte(c)) {
			t.Errorf("Append(%q) must spill rather than drop", c)
		}
	}
	if r.Spilled() != 2 {
		t.Errorf("expected 2 spilled chunks, got %d", r.Spilled())
	}
	if r.Dropped() != 0 {
		t.Errorf("expected 0 drops while spill has room, got %d", r.Dropped())
	}

	// Everything accepted must be readable, in arrival order.
	got := drainString(t, r)
	if got != "abcd" {
		t.Errorf("spilled chunks lost or reordered: got %q, want %q", got, "abcd")
	}
}

// TestChanReaderReadLimitWhenSpillFull verifies that once both the channel
// and the spill are full the reader reports ErrReadLimit — the peer really
// is outrunning twice the configured buffer, which the spill exists to
// bound rather than to hide.
func TestChanReaderReadLimitWhenSpillFull(t *testing.T) {
	r := newChanReader(2, 0, 0) // cap 2, spillMax 2
	for _, c := range []string{"a", "b", "c", "d"} {
		if !r.Append([]byte(c)) {
			t.Fatalf("Append(%q) must be accepted below the limit", c)
		}
	}
	if ok := r.Append([]byte("e")); ok {
		t.Error("expected Append to fail once channel and spill are both full")
	}
	if r.Dropped() != 1 {
		t.Errorf("expected 1 drop, got %d", r.Dropped())
	}
	// The four accepted chunks are still delivered before the error: they
	// arrived before the limit was hit.
	if got := drainString(t, r); got != "abcd" {
		t.Errorf("buffered chunks lost at the limit: got %q, want %q", got, "abcd")
	}
	if _, err := r.Read(make([]byte, 4)); err != ErrReadLimit {
		t.Errorf("expected ErrReadLimit once drained, got %v", err)
	}
}

// TestChanReaderDrainsBufferedChunksBeforeClose is the truncation oracle for
// celeris#484: a reader closed while chunks are still buffered must hand
// those chunks to the handler before reporting the close. Reporting it early
// cuts the stream mid-frame and surfaces as "unexpected EOF". Measured under
// flood, readers were being closed holding a completely full channel.
func TestChanReaderDrainsBufferedChunksBeforeClose(t *testing.T) {
	r := newChanReader(8, 0, 0)
	for _, c := range []string{"one", "two", "three"} {
		if !r.Append([]byte(c)) {
			t.Fatalf("Append(%q) rejected", c)
		}
	}
	// The peer goes away with everything still queued.
	r.closeWith(io.EOF)

	if got := drainString(t, r); got != "onetwothree" {
		t.Errorf("buffered chunks discarded on close: got %q, want %q", got, "onetwothree")
	}
	if _, err := r.Read(make([]byte, 8)); err != io.EOF {
		t.Errorf("expected io.EOF once drained, got %v", err)
	}
}

// TestChanReaderCloseErrorSurvivesDrain verifies the close error is still
// reported after the buffer is drained, not swallowed by the drain path.
func TestChanReaderCloseErrorSurvivesDrain(t *testing.T) {
	r := newChanReader(4, 0, 0)
	r.Append([]byte("tail"))
	want := errors.New("engine read failed")
	r.closeWith(want)

	if got := drainString(t, r); got != "tail" {
		t.Errorf("buffered chunk discarded: got %q", got)
	}
	if _, err := r.Read(make([]byte, 4)); !errors.Is(err, want) {
		t.Errorf("close error lost after drain: got %v, want %v", err, want)
	}
}

// TestChanReaderSpillPreservesOrder is the ordering oracle for the spill
// path: chunks that overflow into the spill must be handed back strictly
// after those already in the channel, with none lost or duplicated. A
// reordering here would corrupt the WebSocket frame stream exactly as a
// dropped chunk would.
func TestChanReaderSpillPreservesOrder(t *testing.T) {
	const capacity = 8
	r := newChanReader(capacity, 0, 0)

	// Fill the channel and then half the spill, interleaving reads so the
	// refill path (spill -> channel tail) is exercised mid-stream.
	var want []byte
	next := byte('A')
	for i := 0; i < capacity+capacity/2; i++ {
		if !r.Append([]byte{next}) {
			t.Fatalf("Append %d rejected before the limit", i)
		}
		want = append(want, next)
		next++
	}
	// Drain two chunks, which promotes spilled chunks into the channel.
	buf := make([]byte, 1)
	var got []byte
	for i := 0; i < 2; i++ {
		n, err := r.Read(buf)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		got = append(got, buf[:n]...)
	}
	// Append more now that room exists; these are later still.
	for i := 0; i < 2; i++ {
		if !r.Append([]byte{next}) {
			t.Fatalf("post-drain Append %d rejected", i)
		}
		want = append(want, next)
		next++
	}
	got = append(got, []byte(drainString(t, r))...)
	if string(got) != string(want) {
		t.Errorf("stream reordered:\n got %q\nwant %q", got, want)
	}
}

// TestChanReaderNoResumeWhileSpilled verifies the reader does not signal
// resume while chunks are still spilled. The buffer is over-full in that
// state, which is the opposite of the drained condition resume means.
func TestChanReaderNoResumeWhileSpilled(t *testing.T) {
	var resumes atomic.Uint32
	r := newChanReader(4, 75, 25) // highWater=3, lowWater=1
	r.SetPauser(func() {}, func() { resumes.Add(1) })

	for i := 0; i < 6; i++ { // 4 into the channel, 2 spilled
		if !r.Append([]byte{byte('a' + i)}) {
			t.Fatalf("Append %d rejected", i)
		}
	}
	if !r.hasSpill() {
		t.Fatal("expected chunks to be spilled")
	}
	// Read down to lowWater. Resume must stay silent while spill is live.
	buf := make([]byte, 1)
	for i := 0; i < 3; i++ {
		if _, err := r.Read(buf); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if r.hasSpill() && resumes.Load() != 0 {
			t.Fatalf("resumed at read %d while %d chunks were still spilled", i, r.spillLen.Load())
		}
	}
}

// buffered reports the chunks currently held by the reader: channel depth
// plus anything spilled behind it. Test-only.
func (r *chanReader) buffered() int {
	return len(r.ch) + int(r.spillLen.Load())
}

// drainString reads exactly the currently buffered chunks (channel depth
// plus spill) so it never blocks waiting for an append that will not come.
func drainString(t *testing.T, r *chanReader) string {
	t.Helper()
	var out []byte
	buf := make([]byte, 64)
	for r.buffered() > 0 || len(r.cur) > 0 {
		n, err := r.Read(buf)
		if err != nil {
			t.Fatalf("drain read: %v", err)
		}
		out = append(out, buf[:n]...)
	}
	return string(out)
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

// TestChanReaderAppendAfterCloseNoPanic verifies Append never panics once
// the reader is closed and returns false. Under the previous close(r.ch)
// design a send racing the close panicked with "send on closed channel";
// with the done-channel design the channel is never closed, so Append that
// observes the close simply drops the chunk.
func TestChanReaderAppendAfterCloseNoPanic(t *testing.T) {
	r := newChanReader(4, 0, 0)
	r.closeWith(io.EOF)
	for i := range 16 {
		if ok := r.Append([]byte{byte(i)}); ok {
			t.Fatalf("Append after close returned true (i=%d)", i)
		}
	}
}

// TestChanReaderAppendCloseRace reproduces the v1.5.7 weekend-soak crash:
// an Append (engine event-loop callback) racing a closeWith (peer-RST error
// handler). Before the done-channel fix, Append's "r.ch <- chunk" could send
// on a channel that closeWith had just closed — "send on closed channel"
// panic, which killed the epoll loop goroutine and tripped I-LIVENESS on
// both arches. Run under -race: the old design also data-races close(r.ch)
// against the send. This must be panic-free and race-clean.
func TestChanReaderAppendCloseRace(t *testing.T) {
	iters := 300
	if testing.Short() {
		iters = 40
	}
	for range iters {
		r := newChanReader(4, 0, 0) // small cap so sends stay live against the drain
		var wg sync.WaitGroup
		start := make(chan struct{})

		// Reader: drains so the "r.ch <- chunk" send path (not just the
		// full/default path) stays live while the close lands underneath it.
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			buf := make([]byte, 8)
			for {
				if _, err := r.Read(buf); err != nil {
					return
				}
			}
		}()

		// Sender: the engine event-loop Append callback. Must not panic
		// when closeWith fires beneath it.
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := range 500 {
				r.Append([]byte{byte(i)})
			}
		}()

		// Closer: the peer-RST SetWSErrorHandler path, interleaved with
		// in-flight sends via a short scheduler spin.
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 20 {
				runtime.Gosched()
			}
			r.closeWith(io.ErrUnexpectedEOF)
		}()

		close(start)
		wg.Wait()
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

// readerPaused reports the reader's own view of the backpressure state,
// read under the mutex that guards it. Test-only; call it only once the
// goroutines under test have joined.
func readerPaused(r *chanReader) bool {
	r.pausedMu.Lock()
	defer r.pausedMu.Unlock()
	return r.pausedState
}

// callbackRanOutsideDecidingLock probes, with no timing whatsoever, whether
// the engine pause/resume callback it is called from was invoked with
// pausedMu RELEASED — the precondition for celeris#667.
//
// chanReader decides to pause/resume under pausedMu and then invokes the
// engine callback after unlocking (engineread.go:231-232 and 312-313), so
// on that code this TryLock always succeeds and the two tests below can
// park the callback to force an interleaving. If the callback is ever
// invoked while the deciding lock is held, TryLock fails, the park is
// skipped, and the tests fall through to the same convergence assertion —
// so neither test can deadlock on a build where the window is closed.
//
// This is used ONLY to schedule the park. It is never the assertion: the
// oracle in both tests is that the engine's state and the reader's state
// agree once everything has quiesced, which leaves the fix free to close
// the window by any means.
func callbackRanOutsideDecidingLock(r *chanReader) bool {
	if !r.pausedMu.TryLock() {
		return false
	}
	r.pausedMu.Unlock()
	return true
}

// TestChanReaderLostResumeConverges is the failing-first oracle for
// celeris#667, the LOST RESUME direction.
//
// chanReader decides to pause under pausedMu but applies the decision to
// the engine after releasing it:
//
//	230		r.pausedState = true
//	231		r.pausedMu.Unlock()
//	232		r.pause()
//
// and Read's resume branch does the same in reverse:
//
//	311			r.pausedState = false
//	312			r.pausedMu.Unlock()
//	313			r.resume()
//
// The engine's closures are Swap-based and ignore the previous value
// (engine/iouring/worker.go:2200-2231), so whichever callback reaches the
// engine LAST wins outright and nothing reconciles. This test forces the
// appending goroutine's pause() to be applied after the draining
// goroutine's resume(), even though the pause was decided first:
//
//  1. the appender crosses highWater, sets pausedState, unlocks, and parks
//     inside pause() before applying it;
//  2. the handler drains to lowWater, sees pausedState == true, clears it,
//     unlocks, and applies resume() — a no-op, the engine is not paused yet;
//  3. the appender wakes and applies pause().
//
// Final state: the engine's recv is PAUSED while the reader believes it is
// RUNNING. The reader will never call resume() again, because from its
// point of view there is nothing to resume, and the connection stops
// delivering inbound data for the rest of its life.
//
// The interleaving is forced by channel handshakes, never by timing: the
// close of `released` inside resume() strictly happens-before pause()'s
// Swap, so the order of the two applications is fixed no matter how the
// scheduler runs the two goroutines.
func TestChanReaderLostResumeConverges(t *testing.T) {
	const chanCap = 8 // highWater=6, lowWater=2
	r := newChanReader(chanCap, 0, 0)

	// desired mirrors the engine's recvPauseDesired: both closures Swap it
	// and ignore the previous value, so its final value is simply whichever
	// callback was applied last.
	var desired atomic.Bool
	var pauses, resumes atomic.Uint32
	var parked, outsideLock atomic.Bool

	entered := make(chan struct{})  // closed by pause() once inside the callback
	released := make(chan struct{}) // closed by resume() once it has applied
	var releaseOnce sync.Once

	pause := func() {
		pauses.Add(1)
		free := callbackRanOutsideDecidingLock(r)
		outsideLock.Store(free)
		close(entered)
		if free {
			parked.Store(true)
			<-released // apply strictly after the handler's resume
		}
		desired.Swap(true)
	}
	resume := func() {
		resumes.Add(1)
		desired.Swap(false)
		releaseOnce.Do(func() { close(released) })
	}
	r.SetPauser(pause, resume)

	var wg sync.WaitGroup

	// The engine worker thread: append until the high-water crossing.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 6 {
			if !r.Append([]byte{byte('a' + i)}) {
				t.Errorf("Append %d rejected below capacity", i)
				return
			}
		}
	}()

	<-entered // the pause has been decided and is in flight

	// The handler goroutine: drain from depth 6 to lowWater (2).
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1)
		for i := range 4 {
			if _, err := r.Read(buf); err != nil {
				t.Errorf("read %d: %v", i, err)
				return
			}
		}
	}()

	wg.Wait()

	// Anti-vacuity guards: the test is worthless unless both callbacks
	// actually fired, and unless the park actually happened on a build
	// where the callback runs outside the deciding lock.
	if got := pauses.Load(); got != 1 {
		t.Fatalf("harness: expected exactly 1 pause callback, got %d", got)
	}
	if got := resumes.Load(); got != 1 {
		t.Fatalf("harness: expected exactly 1 resume callback, got %d", got)
	}
	if outsideLock.Load() && !parked.Load() {
		t.Fatalf("harness: callback ran outside pausedMu but never parked — interleaving was not forced")
	}

	// The oracle: the engine's applied state and the reader's own view of
	// it must agree once both goroutines have quiesced. Nothing re-evaluates
	// after this point, so any disagreement is permanent.
	if engine, reader := desired.Load(), readerPaused(r); engine != reader {
		t.Fatalf("celeris#667: engine recv paused=%v but reader believes paused=%v — "+
			"the resume was applied before the pause it was meant to cancel, so the "+
			"engine stays paused and the reader will never resume it again "+
			"(pauses=%d resumes=%d callbackOutsideLock=%v parked=%v)",
			engine, reader, pauses.Load(), resumes.Load(), outsideLock.Load(), parked.Load())
	}
}

// TestChanReaderParkedResumeConverges is the symmetric direction of
// celeris#667: the resume is parked and a pause decided afterwards lands
// on the engine FIRST, so the resume overwrites it.
//
//  1. the reader is paused (depth crossed highWater), engine paused;
//  2. the handler drains to lowWater, clears pausedState, unlocks, and parks
//     inside resume() before applying it;
//  3. the appender pushes depth back over highWater, sees pausedState ==
//     false, sets it, unlocks, and applies pause();
//  4. the handler wakes and applies resume(), overwriting the pause.
//
// Final state: the engine's recv is RUNNING while the reader believes it is
// PAUSED. This is the benign direction for liveness — data keeps flowing —
// but backpressure is silently gone: the reader is edge-triggered, so it
// will never request another pause, and the peer can now outrun the channel
// and the spill into ErrReadLimit.
//
// As above, the ordering is forced by a channel close, not by timing.
func TestChanReaderParkedResumeConverges(t *testing.T) {
	const chanCap = 8 // highWater=6, lowWater=2
	r := newChanReader(chanCap, 0, 0)

	var desired atomic.Bool
	var pauses, resumes atomic.Uint32
	var parked, outsideLock atomic.Bool

	enteredResume := make(chan struct{})  // closed by resume() once inside
	releasedResume := make(chan struct{}) // closed by the SECOND pause()
	var releaseOnce sync.Once

	pause := func() {
		pauses.Add(1)
		desired.Swap(true)
		// The second pause is the one racing the parked resume; releasing
		// here makes resume's application strictly later than this one.
		if pauses.Load() >= 2 {
			releaseOnce.Do(func() { close(releasedResume) })
		}
	}
	resume := func() {
		resumes.Add(1)
		free := callbackRanOutsideDecidingLock(r)
		outsideLock.Store(free)
		close(enteredResume)
		if free {
			parked.Store(true)
			<-releasedResume // apply strictly after the appender's pause
		}
		desired.Swap(false)
	}
	r.SetPauser(pause, resume)

	// Phase 1, single-goroutine: reach the paused state. No park here —
	// resume has not been called yet and pause never parks in this test.
	for i := range 6 {
		if !r.Append([]byte{byte('a' + i)}) {
			t.Fatalf("Append %d rejected below capacity", i)
		}
	}
	if pauses.Load() != 1 || !desired.Load() || !readerPaused(r) {
		t.Fatalf("harness: expected the paused state after 6 appends: pauses=%d engine=%v reader=%v",
			pauses.Load(), desired.Load(), readerPaused(r))
	}

	var wg sync.WaitGroup

	// The handler goroutine: drain to lowWater, then park inside resume().
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1)
		for i := range 4 {
			if _, err := r.Read(buf); err != nil {
				t.Errorf("read %d: %v", i, err)
				return
			}
		}
	}()

	<-enteredResume // the resume has been decided and is in flight

	// The engine worker thread: push depth back over highWater (2 -> 6).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 4 {
			if !r.Append([]byte{byte('A' + i)}) {
				t.Errorf("post-drain Append %d rejected below capacity", i)
				return
			}
		}
	}()

	wg.Wait()

	if got := pauses.Load(); got != 2 {
		t.Fatalf("harness: expected exactly 2 pause callbacks, got %d", got)
	}
	if got := resumes.Load(); got != 1 {
		t.Fatalf("harness: expected exactly 1 resume callback, got %d", got)
	}
	if outsideLock.Load() && !parked.Load() {
		t.Fatalf("harness: callback ran outside pausedMu but never parked — interleaving was not forced")
	}

	if engine, reader := desired.Load(), readerPaused(r); engine != reader {
		t.Fatalf("celeris#667: engine recv paused=%v but reader believes paused=%v — "+
			"the parked resume overwrote a later pause, so backpressure is gone and "+
			"the edge-triggered reader will never request it again "+
			"(pauses=%d resumes=%d callbackOutsideLock=%v parked=%v)",
			engine, reader, pauses.Load(), resumes.Load(), outsideLock.Load(), parked.Load())
	}
}
