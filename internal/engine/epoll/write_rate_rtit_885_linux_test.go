//go:build linux

package epoll

import (
	"fmt"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#885 item 51, review r3 finding F1: ReadTimeout and IdleTimeout of a
// parked response. The rate rule (parkedBelowRate) is what bounds a response
// the kernel has not taken whole; a client that takes it at or above the floor
// (64 KiB acknowledged per WriteTimeout window) is not to be closed by
// ReadTimeout or IdleTimeout because it sends no frame and no EPOLLOUT edge
// has restarted their clock, whatever their value is next to WriteTimeout
// (the defaults are 60 s, 600 s and 60 s). A conn with no parked response is
// reaped by them as before.
//
// The rig is parked885's (a real TCP pair, 8 MiB queued, no EPOLLOUT edge:
// handleWritable is never called). Times are scaled down; the ratios are the
// point.

// floorRate885 is bytes/s of the floor at WriteTimeout wt, as the tests state it.
func floorRate885(wt time.Duration) float64 {
	return float64(rateFloor885) / wt.Seconds()
}

// riderRun885 is what one run of the paced reader saw.
type riderRun885 struct {
	closedAt  time.Duration // 0: still open at the end
	windows   []int         // bytes the reader took in each WriteTimeout of wall clock from the start
	stoppedAt time.Duration
}

// shortfall reports whether the reader itself fell under the floor (with a
// quarter of margin for the receive buffer) in a full window it was meant to
// be reading in: the apparatus failed, not the engine. stopped windows are not
// counted.
func (r riderRun885) shortfall(upTo int) bool {
	for i := 0; i < upTo && i < len(r.windows); i++ {
		if float64(r.windows[i]) < 1.25*rateFloor885 {
			return true
		}
	}
	return false
}

// ride885 parks a response on the rig with cfg and reads it from the peer at
// mult times the floor of WriteTimeout (a reader that falls behind catches up
// on the next tick, so a stalled test process does not lower the rate the
// engine sees), running the sweep every 25 ms from gap after the start (the
// gap puts the first sweep, which takes the rule's baseline, well after the
// response parked, so that the order of ReadTimeout and the first window end
// does not depend on sweep phase). With stopAfter > 0 the reader stops then.
// It returns when the conn is gone or after total.
func ride885(t *testing.T, cfg resource.Config, mult float64, gap, stopAfter, total time.Duration) riderRun885 {
	t.Helper()
	wt := cfg.WriteTimeout
	r := parked885(t, cfg)
	rate := mult * floorRate885(wt)
	out := riderRun885{windows: make([]int, int(total/wt)+1)}
	start := time.Now()
	var took float64
	for {
		time.Sleep(20 * time.Millisecond)
		el := time.Since(start)
		if el >= total {
			return out
		}
		if stopAfter == 0 || el < stopAfter {
			for due := rate*el.Seconds() - took; due >= 4096; {
				n := r.read(min(int(due), 16<<10))
				if n == 0 {
					break
				}
				took += float64(n)
				due -= float64(n)
				out.windows[int(el/wt)] += n
			}
			// What the kernel took is what the peer can read: the engine's own
			// flush keeps it supplied. flushWrites is no EPOLLOUT edge and no
			// progress for the flush clock.
			_ = r.l.flushWrites(r.cs, true)
		} else if out.stoppedAt == 0 {
			out.stoppedAt = el
		}
		if el >= gap {
			r.l.checkTimeouts()
		}
		if !r.open() {
			out.closedAt = time.Since(start)
			return out
		}
	}
}

// TestParkedReaderAboveTheFloorIsNotCutByReadOrIdleTimeout885: a client taking
// 1.6 and 4 times the floor, no EPOLLOUT edge, with ReadTimeout or IdleTimeout
// equal to WriteTimeout (the defaults' shape: 60 s and 60 s, IdleTimeout ten
// times that), half of it, and both: the rule decides, not the idle clocks,
// which a flush that took bytes or a window passed move and an acknowledged
// byte between two edges does not. Main and the first cut of this item closed
// all of them at ReadTimeout or IdleTimeout (the reviewer's rig: 806 ms at
// RT=WT, 401 ms at RT=WT/2, 811 ms at IT=WT).
func TestParkedReaderAboveTheFloorIsNotCutByReadOrIdleTimeout885(t *testing.T) {
	const wt = 600 * time.Millisecond
	cells := []struct {
		name   string
		rt, it time.Duration
	}{
		{"RT=WT,IT=off", wt, -1},
		{"RT=WT/2,IT=off", wt / 2, -1},
		{"RT=off,IT=WT", -1, wt},
		{"RT=IT=WT", wt, wt},
		{"defaults-shaped RT=WT,IT=10WT", wt, 10 * wt},
	}
	for _, c := range cells {
		for _, mult := range []float64{1.6, 4} {
			t.Run(fmt.Sprintf("%s/%.1fx", c.name, mult), func(t *testing.T) {
				var run riderRun885
				for attempt := 1; attempt <= 3; attempt++ {
					run = ride885(t, resource.Config{ReadTimeout: c.rt, IdleTimeout: c.it, WriteTimeout: wt},
						mult, 100*time.Millisecond, 0, 3*wt)
					if run.closedAt == 0 || !run.shortfall(int(run.closedAt/wt)) {
						break
					}
					t.Logf("attempt %d: the reader itself took %v bytes per window, under the floor: retrying", attempt, run.windows)
				}
				if run.closedAt != 0 {
					t.Fatalf("celeris#885 item 51: reaped %v in, the reader taking %.1fx the floor (%d KiB per %v window; "+
						"taken per window: %v), no EPOLLOUT edge, ReadTimeout %v IdleTimeout %v WriteTimeout %v: "+
						"the rate rule decides a parked response, not ReadTimeout or IdleTimeout",
						run.closedAt.Round(time.Millisecond), mult, rateFloor885>>10, wt, run.windows, c.rt, c.it, wt)
				}
			})
		}
	}
}

// TestParkedReaderThatStopsIsClosedWithinTwoWindowsWhateverReadTimeout885: a
// client at 4 times the floor for two windows that then takes nothing is closed
// by the rule within two windows of its last byte (a window it passed holds
// the conn one more), with ReadTimeout and IdleTimeout shorter than, equal to,
// and longer than WriteTimeout: the rule's bound is WriteTimeout's, not the idle clocks'.
func TestParkedReaderThatStopsIsClosedWithinTwoWindowsWhateverReadTimeout885(t *testing.T) {
	const wt = 600 * time.Millisecond
	for _, c := range []struct {
		name   string
		rt, it time.Duration
	}{{"RT=WT/2,IT=off", wt / 2, -1}, {"RT=IT=WT", wt, wt}, {"RT=off,IT=off", -1, -1}} {
		t.Run(c.name, func(t *testing.T) {
			const stop = 2 * wt
			run := ride885(t, resource.Config{ReadTimeout: c.rt, IdleTimeout: c.it, WriteTimeout: wt}, 4, 100*time.Millisecond, stop, stop+4*wt)
			if run.closedAt == 0 {
				t.Fatalf("a reader that stopped after %v was still held %v later (ReadTimeout %v IdleTimeout %v WriteTimeout %v)", stop, 4*wt, c.rt, c.it, wt)
			}
			if run.closedAt < stop || run.closedAt > stop+2*wt+250*time.Millisecond {
				t.Errorf("closed at %v with the reader stopped at %v (taken per window: %v), want within two windows (%v) of it, and not before",
					run.closedAt.Round(time.Millisecond), stop, run.windows, 2*wt)
			}
		})
	}
}

// TestParkedSecondResponseRightAfterTheFirstIsRateGoverned885: a keep-alive
// conn that parks a second large response right after the first drained, with
// ReadTimeout (150 ms) below a quarter of WriteTimeout (800 ms), and a client
// taking 4 times the floor, is held. The window baseline of the first
// response must not delay the second's: nothing was sampled for it yet, and
// ReadTimeout runs on it until the rule has a baseline.
func TestParkedSecondResponseRightAfterTheFirstIsRateGoverned885(t *testing.T) {
	const wt = 800 * time.Millisecond
	r := parked885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: -1, WriteTimeout: wt})
	rate := 4 * floorRate885(wt)
	start := time.Now()
	var took float64
	tick := func() bool {
		time.Sleep(20 * time.Millisecond)
		for due := rate*time.Since(start).Seconds() - took; due >= 4096; {
			n := r.read(min(int(due), 16<<10))
			if n == 0 {
				break
			}
			took += float64(n)
			due -= float64(n)
		}
		_ = r.l.flushWrites(r.cs, true)
		r.l.checkTimeouts()
		return r.open()
	}
	// The first response: until a window has passed (the sample the second
	// response's start must not inherit).
	passed := false
	var base int64
	for time.Since(start) < 3*wt && !passed {
		if !tick() {
			t.Fatalf("apparatus: the first response's conn was reaped %v in", time.Since(start).Round(time.Millisecond))
		}
		if r.cs.closeAckedKnown {
			if base == 0 {
				base = r.cs.closeSampled
			} else if r.cs.closeSampled != base {
				passed = true // a window passed: the sample just moved
			}
		}
	}
	if !passed {
		t.Fatalf("apparatus: no window passed in %v", 3*wt)
	}
	// The first response ends (the drain site's own call) and a second is
	// queued and parked at once.
	r.cs.writeBuf = r.cs.writeBuf[:0]
	r.cs.writePos = 0
	r.cs.endWrite()
	r.cs.writeBuf = append(r.cs.writeBuf, make([]byte, 8<<20)...)
	_ = r.l.flushWrites(r.cs, true)
	r.cs.pendingBytes = csPendingBytes(r.cs)
	if !csWritePending(r.cs) {
		t.Fatalf("apparatus: the second response was taken whole")
	}
	r.l.armEpollOut(r.cs)
	r.l.cfg.ReadTimeout = 150 * time.Millisecond // the first response ran under an hour
	second := time.Now()
	for time.Since(second) < 3*wt {
		if !tick() {
			t.Fatalf("celeris#885 item 51: the second response's conn was reaped %v after it parked, the reader taking 4x the floor, "+
				"ReadTimeout 150 ms (under WriteTimeout/4: the stale sample of the first response left it with no baseline)",
				time.Since(second).Round(time.Millisecond))
		}
	}
}

// TestParkedConnWithNothingParkedIsStillReapedByReadTimeout885: the other face
// of F1, a guard that holds on main and must go on holding: a conn whose
// response the client has taken whole and that then sits idle is reaped by
// ReadTimeout (300 ms) from the end of the response with WriteTimeout 600 ms
// on a TCP socket, and one whose socket does not report acknowledged bytes
// (an AF_UNIX pair, as the 876 rig has) and holds a response nobody takes is
// reaped by ReadTimeout from the last progress, not held for WriteTimeout (an
// hour).
func TestParkedConnWithNothingParkedIsStillReapedByReadTimeout885(t *testing.T) {
	t.Run("tcp-drained-then-idle", func(t *testing.T) {
		const rt = 300 * time.Millisecond
		r := newTCPClosingRig876(t, resource.Config{ReadTimeout: rt, IdleTimeout: -1, WriteTimeout: 600 * time.Millisecond}, 1<<20)
		r.cs.lastActivity = time.Now().UnixNano()
		r.l.armEpollOut(r.cs)
		for i := 0; i < 400 && csWritePending(r.cs); i++ {
			for r.read(256<<10) > 0 {
			}
			r.l.handleWritable(r.cs)
		}
		if csWritePending(r.cs) || r.cs.writeSince != 0 {
			t.Fatalf("apparatus: the response did not drain (writeSince=%d)", r.cs.writeSince)
		}
		drained := time.Now()
		for time.Since(drained) < 2*time.Second {
			time.Sleep(20 * time.Millisecond)
			r.l.checkTimeouts()
			if !r.open() {
				if gone := time.Since(drained); gone < rt || gone > rt+250*time.Millisecond {
					t.Errorf("an idle conn was reaped %v after its response ended, want ReadTimeout %v", gone.Round(time.Millisecond), rt)
				}
				return
			}
		}
		t.Fatalf("an idle keep-alive conn was not reaped by ReadTimeout %v, 2 s after its response ended", rt)
	})
	t.Run("not-tcp-parked", func(t *testing.T) {
		const rt = 300 * time.Millisecond
		r := newClosingRig876(t, resource.Config{ReadTimeout: rt, IdleTimeout: -1, WriteTimeout: time.Hour}, 1<<20)
		r.cs.lastActivity = time.Now().UnixNano()
		r.l.armEpollOut(r.cs)
		start := time.Now()
		for time.Since(start) < 2*time.Second {
			time.Sleep(20 * time.Millisecond)
			r.l.checkTimeouts()
			if !r.open() {
				if gone := time.Since(start); gone < rt || gone > rt+250*time.Millisecond {
					t.Errorf("a parked conn whose socket reports no acknowledged bytes was reaped %v in, want ReadTimeout %v", gone.Round(time.Millisecond), rt)
				}
				return
			}
		}
		t.Fatalf("a parked conn with no acknowledged-bytes signal was not reaped by ReadTimeout %v, 2 s in", rt)
	})
}

// TestParkedResponseDrainedBehindTheLoopStartsTheIdleClockAtTheSweep885: a
// response the rule governs (a baseline is known, writeSince is the last
// window's stamp) that the dispatch goroutine's own flush drains, which the
// loop sees at a sweep (the queue is found empty), starts ReadTimeout from that
// sweep and not from the stamp of a window passed long before: a conn is not
// reaped at the instant its response ended because ReadTimeout (200 ms) is
// shorter than the window (1.6 s) it was last stamped in.
func TestParkedResponseDrainedBehindTheLoopStartsTheIdleClockAtTheSweep885(t *testing.T) {
	const wt = 1600 * time.Millisecond
	r := parked885(t, resource.Config{ReadTimeout: time.Hour, IdleTimeout: -1, WriteTimeout: wt})
	r.l.checkTimeouts() // the baseline
	if !r.cs.closeAckedKnown {
		t.Fatalf("apparatus: no baseline after the first sweep")
	}
	r.l.cfg.ReadTimeout = 200 * time.Millisecond
	time.Sleep(500 * time.Millisecond) // the stamp is now older than ReadTimeout
	r.l.checkTimeouts()
	if !r.open() {
		t.Fatalf("the rule governs a parked response; ReadTimeout reaped it")
	}
	// The goroutine's flush emptied the queue; the loop has not seen it.
	r.cs.writeBuf = r.cs.writeBuf[:0]
	r.cs.writePos = 0
	r.cs.pendingBytes = 0
	r.l.checkTimeouts()
	if !r.open() {
		t.Fatalf("the conn was reaped at the sweep that found its response drained: ReadTimeout ran from the stamp of a window passed 500 ms before")
	}
	if r.cs.writeSince != 0 || r.cs.closeAckedKnown {
		t.Errorf("the sweep left the clock running (writeSince=%d known=%v)", r.cs.writeSince, r.cs.closeAckedKnown)
	}
}
