//go:build linux

package epoll

import (
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#885 items 35 and 45: the unit side of the write clock
// (connState.writeSince), on the rig of celeris#876 with a real TCP pair (the
// peer has a 16 KiB receive buffer, the conn a 256 KiB send buffer; an 8 MiB
// response is queued). The rig drives the real armEpollOut, handleWritable,
// relink, flushWrites and checkTimeouts, the engine's own partial-flush
// bookkeeping included. The end-to-end halves are in
// keepalive_writeprogress_885_linux_test.go.

// parked885 is a rig whose conn is parked as the engine parks a keep-alive
// conn whose response the kernel did not take whole: armEpollOut called, the
// queue partly sent.
func parked885(t *testing.T, cfg resource.Config) *closingRig876 {
	t.Helper()
	r := newTCPClosingRig876(t, cfg, 8<<20)
	r.cs.lastActivity = time.Now().UnixNano()
	if r.cs.writeSince != 0 {
		t.Fatalf("celeris885 PREMISE: the rig's first flush started the clock")
	}
	r.l.armEpollOut(r.cs)
	return r
}

// TestWriteClockStartsAtThePartialFlushAndBoundsWriteTimeout885: armEpollOut
// starts the clock, and a peer that takes nothing is reaped WriteTimeout later,
// whatever ReadTimeout and IdleTimeout (an hour) and the last read say.
func TestWriteClockStartsAtThePartialFlushAndBoundsWriteTimeout885(t *testing.T) {
	const bound = 300 * time.Millisecond
	r := parked885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: bound})
	if r.cs.writeSince == 0 || !r.cs.epollOut {
		t.Fatalf("armEpollOut left writeSince=%d epollOut=%v, want the clock started and EPOLLOUT armed", r.cs.writeSince, r.cs.epollOut)
	}
	started := r.cs.writeSince
	r.l.armEpollOut(r.cs) // a second partial flush of the same response must not restart it
	if r.cs.writeSince != started {
		t.Errorf("a second armEpollOut moved the clock from %d to %d", started, r.cs.writeSince)
	}
	r.l.checkTimeouts()
	if !r.open() {
		t.Fatalf("the conn was reaped at once, before WriteTimeout")
	}
	time.Sleep(bound + 100*time.Millisecond)
	r.l.checkTimeouts()
	if r.open() {
		t.Fatalf("celeris#885 item 45: a parked conn whose peer took nothing for %v was not reaped by WriteTimeout %v", bound+100*time.Millisecond, bound)
	}
}

// TestWriteClockRestartsWhenTheFlushTakesBytes885: a peer taking 64 KiB every
// 100 ms is never reaped by a WriteTimeout of 300 ms (no progress for it), over
// five times the bound, and is reaped once it stops. The flush is
// handleWritable, as an EPOLLOUT edge calls it.
func TestWriteClockRestartsWhenTheFlushTakesBytes885(t *testing.T) {
	const bound = 300 * time.Millisecond
	r := parked885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: bound})
	start := time.Now()
	for time.Since(start) < 5*bound {
		time.Sleep(100 * time.Millisecond)
		if r.read(64<<10) == 0 {
			t.Fatalf("apparatus: the peer read nothing")
		}
		r.l.handleWritable(r.cs)
		r.l.checkTimeouts()
		if !r.open() {
			t.Fatalf("celeris#885 item 45: the conn was reaped %v in, with the peer taking 64 KiB every 100 ms and WriteTimeout %v: "+
				"the clock must restart when a flush takes bytes", time.Since(start).Round(time.Millisecond), bound)
		}
	}
	time.Sleep(bound + 100*time.Millisecond)
	r.l.checkTimeouts()
	if r.open() {
		t.Fatalf("a peer that stopped reading was not reaped %v after its last byte", bound+100*time.Millisecond)
	}
}

// TestReadTimeoutOfAParkedConnRunsFromTheWriteClock885: the last read is an
// hour old and ReadTimeout is 300 ms; a parked conn is neither reaped on arrival
// (the response is being taken) nor held beyond ReadTimeout from the last
// progress (WriteTimeout is off, so ReadTimeout is the only bound a parked
// conn has, as it was before the clock).
func TestReadTimeoutOfAParkedConnRunsFromTheWriteClock885(t *testing.T) {
	const bound = 300 * time.Millisecond
	r := newTCPClosingRig876(t, resource.Config{ReadTimeout: bound, IdleTimeout: -1, WriteTimeout: -1}, 8<<20)
	r.cs.lastActivity = time.Now().Add(-time.Hour).UnixNano()
	r.l.armEpollOut(r.cs)
	r.l.checkTimeouts()
	if !r.open() {
		t.Fatalf("celeris#885 item 35: a conn whose response started to go out a moment ago was reaped on a ReadTimeout measured from a read an hour old")
	}
	for i := 0; i < 8; i++ { // 800 ms of steady taking, over twice the bound
		time.Sleep(100 * time.Millisecond)
		r.read(64 << 10)
		r.l.handleWritable(r.cs)
		r.l.checkTimeouts()
		if !r.open() {
			t.Fatalf("celeris#885 item 35: reaped %d ms in, the peer taking 64 KiB every 100 ms, ReadTimeout %v", (i+1)*100, bound)
		}
	}
	time.Sleep(bound + 100*time.Millisecond)
	r.l.checkTimeouts()
	if r.open() {
		t.Fatalf("a parked conn whose peer stopped taking its response was not reaped by ReadTimeout %v", bound)
	}
}

// TestWriteClockEndsAtTheDrainAndFoldsIntoLastActivity885: when the peer has
// taken the whole response the clock is gone and the idle clock of the next
// request starts there, not at the request before the response.
func TestWriteClockEndsAtTheDrainAndFoldsIntoLastActivity885(t *testing.T) {
	r := newTCPClosingRig876(t, resource.Config{ReadTimeout: 300 * time.Millisecond, IdleTimeout: -1, WriteTimeout: -1}, 1<<20)
	r.cs.lastActivity = time.Now().Add(-time.Hour).UnixNano()
	r.l.armEpollOut(r.cs)
	for i := 0; i < 400 && csWritePending(r.cs); i++ {
		for r.read(256<<10) > 0 {
		}
		r.l.handleWritable(r.cs)
	}
	if csWritePending(r.cs) {
		t.Fatalf("apparatus: the response did not drain")
	}
	if r.cs.writeSince != 0 || r.cs.epollOut {
		t.Errorf("after the drain writeSince=%d epollOut=%v, want 0 and disarmed", r.cs.writeSince, r.cs.epollOut)
	}
	if since := time.Since(time.Unix(0, r.cs.lastActivity)); since > time.Second {
		t.Errorf("lastActivity is %v old after the drain: the idle clock of the next request must start at the end of the response", since)
	}
	r.l.checkTimeouts()
	if !r.open() {
		t.Fatalf("the conn was reaped right after its response drained, on a ReadTimeout measured from the request before it")
	}
}

// TestSweepEndsTheClockOfAConnWithNothingQueued885: a drain the loop did not
// see (an async conn's dispatch goroutine flushes under its own lock) leaves
// the clock set with an empty queue. The sweep must end it and not apply
// WriteTimeout to a conn that has nothing to write.
func TestSweepEndsTheClockOfAConnWithNothingQueued885(t *testing.T) {
	r := newTCPClosingRig876(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: 100 * time.Millisecond}, 1<<20)
	r.l.armEpollOut(r.cs)
	for i := 0; i < 400 && csWritePending(r.cs); i++ {
		for r.read(256<<10) > 0 {
		}
		_ = r.l.flushWrites(r.cs, true) // the goroutine's flush: no handleWritable, no endWrite
	}
	if csWritePending(r.cs) {
		t.Fatalf("apparatus: the response did not drain")
	}
	if r.cs.writeSince == 0 {
		t.Fatalf("apparatus: the clock is not set")
	}
	time.Sleep(200 * time.Millisecond)
	r.l.checkTimeouts()
	if !r.open() {
		t.Fatalf("a conn with nothing queued was reaped by WriteTimeout from a stale write clock")
	}
	if r.cs.writeSince != 0 {
		t.Errorf("the sweep left writeSince=%d on a conn with nothing queued", r.cs.writeSince)
	}
}

// TestWriteClockSurvivesARelinkAndIsNotMovedByFlushWrites885: relink disarms
// EPOLLOUT while bytes are still queued, and flushWrites, which every
// response's first flush goes through, takes bytes without being evidence the
// peer is reading in the sense of the clock. Neither may restart it, or a
// pipelining client that never reads would get a fresh WriteTimeout for every
// request it sends.
func TestWriteClockSurvivesARelinkAndIsNotMovedByFlushWrites885(t *testing.T) {
	r := parked885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: time.Hour})
	r.cs.writeSince -= int64(10 * time.Second)
	old := r.cs.writeSince
	r.l.relink(r.cs)
	if r.cs.writeSince != old {
		t.Errorf("relink moved the clock from %d to %d while bytes are queued", old, r.cs.writeSince)
	}
	r.l.armEpollOut(r.cs)
	r.read(64 << 10)
	_ = r.l.flushWrites(r.cs, true)
	if r.cs.writeSince != old {
		t.Errorf("flushWrites moved the clock from %d to %d", old, r.cs.writeSince)
	}
}

// TestFlushDirtyRestartsTheWriteClockOnlyWhenItTakesBytes885: the dirty pass is
// the other partial-flush site (a relinked or async conn reaches it with its
// response). A pass that takes bytes restarts the clock; one that finds the
// socket full (EAGAIN, nothing taken) leaves it where it was.
func TestFlushDirtyRestartsTheWriteClockOnlyWhenItTakesBytes885(t *testing.T) {
	r := parked885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: time.Hour, WriteTimeout: time.Hour})
	stale := r.cs.writeSince - int64(10*time.Second)
	r.cs.writeSince = stale

	r.l.markDirty(r.cs)
	r.l.flushDirty() // the peer has read nothing: the socket is full
	if r.cs.writeSince != stale {
		t.Errorf("a dirty pass that took no bytes moved the clock from %d to %d", stale, r.cs.writeSince)
	}

	r.l.markDirty(r.cs)
	r.read(256 << 10)
	r.l.flushDirty()
	if r.cs.writeSince == stale || time.Since(time.Unix(0, r.cs.writeSince)) > time.Second {
		t.Errorf("a dirty pass that took bytes left the clock at %d (%v old), want it restarted", r.cs.writeSince, time.Since(time.Unix(0, r.cs.writeSince)))
	}
}
