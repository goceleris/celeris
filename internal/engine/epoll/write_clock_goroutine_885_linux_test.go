//go:build linux

package epoll

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#885 items 35 and 45, review round 1: bytes the dispatch goroutine's own
// flush puts into the socket after the peer took some are progress the write
// clock must see. The dirty pass that follows the hand-back finds the socket
// full again (queued == pending across its one flush), so without the goroutine
// noting it (resyncPending, goMoved) the clock stays 0.9 WriteTimeout old and
// the sweep closes a conn whose client read a moment ago. The pipelined
// request's read is fresh, and main measured ReadTimeout and IdleTimeout from it
// and applied no WriteTimeout to a parked conn.
func TestGoroutineFlushThatTakesBytesRestartsTheWriteClock885(t *testing.T) {
	const bound = time.Second
	r := parked885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: bound})
	r.cs.writeSince = time.Now().Add(-9 * bound / 10).UnixNano()
	r.cs.lastActivity = time.Now().UnixNano() // the pipelined request's read
	got := 0
	for i := 0; i < 8; i++ {
		got += r.read(64 << 10)
	}
	if got == 0 {
		t.Fatalf("apparatus: the peer read nothing")
	}
	r.cs.pendingBytes = csPendingBytes(r.cs) // what the write function left it at
	before := csPendingBytes(r.cs)
	_ = r.l.flushWrites(r.cs, false) // the goroutine's flush after its handler: takes bytes
	r.cs.resyncPending()             // the goroutine's resync, under detachMu
	after := csPendingBytes(r.cs)
	if before-after <= 0 {
		t.Fatalf("apparatus: the goroutine's flush took nothing (%d before, %d after)", before, after)
	}
	if !r.cs.goMoved {
		t.Fatalf("a flush that took %d bytes and left the rest queued did not note goMoved", before-after)
	}
	r.l.markDirty(r.cs)
	r.l.flushDirty() // the hand-back: finds the socket full, takes nothing
	if age := time.Since(time.Unix(0, r.cs.writeSince)); age > bound/2 {
		t.Errorf("the write clock is %v old after the goroutine's flush took %d bytes: want it restarted", age.Round(time.Millisecond), before-after)
	}
	if r.cs.goMoved {
		t.Errorf("the loop's partial flush left goMoved set")
	}
	time.Sleep(200 * time.Millisecond)
	r.l.checkTimeouts()
	if !r.open() {
		t.Errorf("a conn whose goroutine flush took %d bytes 200 ms ago was closed by WriteTimeout %v", before-after, bound)
	}
}

// TestGoroutineFlushThatTakesNothingDoesNotRestartTheWriteClock885: the
// converse. A goroutine flush into a full socket (the response it appended is
// still all queued) is not progress, and neither is a stale goMoved left by
// an earlier response once the loop has seen one partial flush.
func TestGoroutineFlushThatTakesNothingDoesNotRestartTheWriteClock885(t *testing.T) {
	const bound = time.Second
	r := parked885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: bound})
	stamp := time.Now().Add(-9 * bound / 10).UnixNano()
	r.cs.writeSince = stamp
	r.cs.pendingBytes = csPendingBytes(r.cs)
	_ = r.l.flushWrites(r.cs, false) // the peer has read nothing: the socket is full
	r.cs.resyncPending()
	if r.cs.goMoved {
		t.Fatalf("a flush into a full socket noted goMoved")
	}
	r.l.markDirty(r.cs)
	r.l.flushDirty()
	if r.cs.writeSince != stamp {
		t.Errorf("a goroutine flush and a loop flush that took nothing moved the clock from %d to %d", stamp, r.cs.writeSince)
	}
	// A stale goMoved (set, then the queue drained by the loop) is consumed
	// by the first partial flush of the next response, which starts its own
	// clock and so is not extended by it.
	r.cs.goMoved = true
	r.cs.writeSince = 0
	r.l.markDirty(r.cs)
	r.l.flushDirty()
	if r.cs.goMoved {
		t.Errorf("a partial flush on a conn with no clock left goMoved set")
	}
}

// TestGoroutineFlushThatEmptiesTheQueueIsSeenByTheWriteClock885 (review round
// 2): resyncPending's drained branch noted nothing, so the dispatch
// goroutine's flush that emptied a clocked queue was invisible to the clock.
// The next response, appended by the goroutine, finds the socket full again
// and takes nothing, so no flush restarts the clock, and the clock stays as
// old as the response the peer finished taking a moment ago.
func TestGoroutineFlushThatEmptiesTheQueueIsSeenByTheWriteClock885(t *testing.T) {
	const bound = 300 * time.Millisecond
	r := parked885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: bound})
	time.Sleep(bound * 2 / 3) // the loop sees no progress meanwhile (a pipelined handler runs)
	goroutineFlush := func(n int) {
		for r.read(n) > 0 && n >= 256<<10 {
		}
		r.cs.pendingBytes = csPendingBytes(r.cs)
		_ = r.l.flushWrites(r.cs, false) // the goroutine's flush
		r.cs.resyncPending()
	}
	for i := 0; i < 20000 && csPendingBytes(r.cs) > 64<<10; i++ {
		goroutineFlush(32 << 10) // a little room at a time, so that the queue is not emptied by accident
	}
	// The partial flushes above noted goMoved, as they should; what is under
	// test is the flush that EMPTIES the queue, so the mark starts clear.
	r.cs.goMoved = false
	if csPendingBytes(r.cs) == 0 {
		t.Fatalf("apparatus: the queue is empty already")
	}
	goroutineFlush(256 << 10)
	if csWritePending(r.cs) {
		t.Fatalf("apparatus: did not drain")
	}
	drainedAt := time.Now()
	// The socket is full again before the next response (the peer stops reading
	// here): top it up, so that the next flush takes nothing.
	r.cs.writeBuf = append(r.cs.writeBuf[:0], make([]byte, 8<<20)...)
	r.cs.writePos = 0
	_ = r.l.flushWrites(r.cs, false)
	r.cs.writeBuf, r.cs.writePos = r.cs.writeBuf[:0], 0
	r.cs.pendingBytes = 0
	// The next response, appended by the goroutine; its flush takes nothing.
	r.cs.writeBuf = append(r.cs.writeBuf, make([]byte, 1<<20)...)
	r.cs.pendingBytes = csPendingBytes(r.cs)
	_ = r.l.flushWrites(r.cs, false)
	r.cs.resyncPending()
	r.l.markDirty(r.cs)
	r.l.flushDirty()
	age := time.Since(time.Unix(0, r.cs.writeSince))
	sinceDrain := time.Since(drainedAt)
	if age > sinceDrain+50*time.Millisecond {
		t.Errorf("the write clock (%v old) predates the goroutine's draining flush (%v ago): that progress was not seen",
			age.Round(time.Millisecond), sinceDrain.Round(time.Millisecond))
	}
}
