//go:build linux

package epoll

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#885 items 35 and 45, review round 1: a conn can be left with EPOLLOUT
// armed and no write clock. The drain sites (flushDirty's drained branch, the
// HTTP/2 queue pass, drainRead's body-recv branch, the sweep's empty-queue
// branch) end the clock without disarming EPOLLOUT, and armEpollOut returned
// on cs.epollOut before it stamped, so the next response's partial flush parked
// the conn with no clock at all and WriteTimeout was not applied to it.

// TestPartialFlushAfterADrainThatLeftEPOLLOUTArmedStartsTheClock885: the
// dispatch goroutine hands a conn back (drainDetachQueue marks it dirty with no
// epollOut check) and flushDirty drains it with EPOLLOUT still armed; the next
// response, flushed partly by the same pass, must be clocked.
func TestPartialFlushAfterADrainThatLeftEPOLLOUTArmedStartsTheClock885(t *testing.T) {
	const bound = 300 * time.Millisecond
	r := newTCPClosingRig876(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: bound}, 1<<20)
	r.cs.lastActivity = time.Now().UnixNano()
	r.l.armEpollOut(r.cs)
	if r.cs.writeSince == 0 || !r.cs.epollOut {
		t.Fatalf("premise: armEpollOut did not park the conn")
	}
	// The peer takes the whole response; the dirty pass is the flush that
	// drains it.
	for i := 0; i < 400 && csWritePending(r.cs); i++ {
		for r.read(256<<10) > 0 {
		}
		r.l.markDirty(r.cs)
		r.l.flushDirty()
	}
	if csWritePending(r.cs) {
		t.Fatalf("apparatus: the response did not drain")
	}
	if !r.cs.epollOut {
		t.Fatalf("premise: the dirty pass disarmed EPOLLOUT, the state under test needs it armed")
	}
	// The next response, larger than the socket takes, before any EPOLLOUT edge
	// (the peer has stopped reading).
	r.cs.writeBuf = append(r.cs.writeBuf, make([]byte, 8<<20)...)
	r.l.markDirty(r.cs)
	r.l.flushDirty()
	if !csWritePending(r.cs) {
		t.Fatalf("apparatus: the second response went out whole")
	}
	if r.cs.writeSince == 0 {
		t.Errorf("celeris#885 items 35, 45: the second response is parked (epollOut=%v, %d bytes queued) with no write clock", r.cs.epollOut, csPendingBytes(r.cs))
	}
	time.Sleep(bound + 200*time.Millisecond)
	r.l.checkTimeouts()
	if r.open() {
		t.Errorf("celeris#885 item 45: a parked conn whose peer took nothing for %v is still open with WriteTimeout %v (writeSince=%d epollOut=%v)",
			bound+200*time.Millisecond, bound, r.cs.writeSince, r.cs.epollOut)
	}
}

// TestPartialFlushAfterTheSweepEndedTheClockStartsTheClock885: the state
// TestSweepEndsTheClockOfAConnWithNothingQueued885 builds (armed, the dispatch
// goroutine's own flush drained the queue, the sweep ended the clock), then the
// goroutine's next response, which it flushes partly and hands back
// (drainDetachQueue, markDirty, flushDirty into a full socket).
func TestPartialFlushAfterTheSweepEndedTheClockStartsTheClock885(t *testing.T) {
	const bound = 300 * time.Millisecond
	r := newTCPClosingRig876(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: bound}, 1<<20)
	r.cs.lastActivity = time.Now().UnixNano()
	r.l.armEpollOut(r.cs)
	for i := 0; i < 400 && csWritePending(r.cs); i++ {
		for r.read(256<<10) > 0 {
		}
		_ = r.l.flushWrites(r.cs, false) // the goroutine's flush
	}
	if csWritePending(r.cs) {
		t.Fatalf("apparatus: the response did not drain")
	}
	r.l.checkTimeouts() // ends the stale clock
	if r.cs.writeSince != 0 || !r.cs.epollOut || !r.open() {
		t.Fatalf("premise: after the sweep writeSince=%d epollOut=%v open=%v, want 0, true, true", r.cs.writeSince, r.cs.epollOut, r.open())
	}
	r.cs.writeBuf = append(r.cs.writeBuf, make([]byte, 8<<20)...)
	_ = r.l.flushWrites(r.cs, false)
	r.l.markDirty(r.cs)
	r.l.flushDirty()
	if !csWritePending(r.cs) {
		t.Fatalf("apparatus: the next response went out whole")
	}
	if r.cs.writeSince == 0 {
		t.Errorf("celeris#885 items 35, 45: the next response is parked (epollOut=%v, %d bytes queued) with no write clock", r.cs.epollOut, csPendingBytes(r.cs))
	}
	time.Sleep(bound + 200*time.Millisecond)
	r.l.checkTimeouts()
	if r.open() {
		t.Errorf("celeris#885 item 45: a parked conn whose peer took nothing for %v is still open with WriteTimeout %v (writeSince=%d epollOut=%v)",
			bound+200*time.Millisecond, bound, r.cs.writeSince, r.cs.epollOut)
	}
}

// TestWritableEdgeOfAClocklessArmedConnStartsTheClock885: handleWritable's
// partial branch does not reach armEpollOut, so an armed conn with bytes queued
// and no clock (a drain site ended the clock, then a response was queued
// without a pass through armEpollOut) must be stamped there.
func TestWritableEdgeOfAClocklessArmedConnStartsTheClock885(t *testing.T) {
	const bound = 300 * time.Millisecond
	r := parked885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: bound})
	r.cs.writeSince = 0
	if r.read(64<<10) == 0 {
		t.Fatalf("apparatus: the peer read nothing")
	}
	r.l.handleWritable(r.cs) // the EPOLLOUT edge: takes some, leaves the rest
	if !csWritePending(r.cs) {
		t.Fatalf("apparatus: the response drained")
	}
	if r.cs.writeSince == 0 {
		t.Fatalf("celeris#885 items 35, 45: a partial handleWritable left an armed conn with %d bytes queued and no write clock", csPendingBytes(r.cs))
	}
	time.Sleep(bound + 200*time.Millisecond)
	r.l.checkTimeouts()
	if r.open() {
		t.Errorf("a parked conn whose peer took nothing for %v is still open with WriteTimeout %v", bound+200*time.Millisecond, bound)
	}
}
