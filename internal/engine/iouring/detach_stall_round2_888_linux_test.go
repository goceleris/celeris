//go:build linux

package iouring

import (
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
)

// Review round 1 of celeris#1077: the stall bound's reach (a SEND of any size
// of a detached conn is watched), the first sample of a stall met late, the
// look's rate limit and the drain after a stall close.

// TestDetachedSendOfAnySizeIsWatched888: the SEND of the leftover of a
// detached stream's inline write is smaller than longSendBytes, and a producer
// that goes quiet after it leaves the worker with nothing to wake it. Its SEND
// must be marked (remainderWatch: the timed wait, sendWatch: the sweep) for the
// stall bound to run. Not for a conn that is not truly detached, or on a server
// without a WriteTimeout.
func TestDetachedSendOfAnySizeIsWatched888(t *testing.T) {
	mk := func(wt time.Duration, detached, mu bool) (*Worker, *connState) {
		w := &Worker{}
		w.cfg.WriteTimeout = wt
		cs := &connState{h1State: conn.NewH1State()}
		cs.h1State.Detached.Store(detached)
		if mu {
			cs.detachMu = &sync.Mutex{}
		}
		return w, cs
	}
	cases := []struct {
		name     string
		wt       time.Duration
		detached bool
		mu       bool
		n        int
		want     bool
	}{
		{"detached-4KiB", time.Second, true, true, 4 << 10, true},
		{"detached-1-byte", time.Second, true, true, 1, true},
		{"detached-long", time.Second, true, true, longSendBytes, true},
		{"detached-no-write-timeout", 0, true, true, 4 << 10, false},
		{"detached-write-timeout-off", -1, true, true, 4 << 10, false},
		{"async-conn-not-detached", time.Second, false, true, 4 << 10, false},
		{"inline-conn", time.Second, false, false, 4 << 10, false},
		{"long-but-not-detached", time.Second, false, false, longSendBytes, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, cs := mk(c.wt, c.detached, c.mu)
			w.markLongSend(cs, c.n)
			if cs.remainderOut != c.want || w.remainderWatch != c.want || w.sendWatch != c.want {
				t.Fatalf("marked=%v remainderWatch=%v sendWatch=%v, want %v", cs.remainderOut, w.remainderWatch, w.sendWatch, c.want)
			}
			if got := waitUntimed(true, true, false, w.remainderWatch); got == c.want {
				t.Fatalf("waitUntimed = %v with remainderWatch %v: the worker waits with no timeout for a SEND it must look at", got, w.remainderWatch)
			}
		})
	}
}

// TestDetachedSparseSweepsGiveAReadingPeerABound888: sweeps further apart than
// WriteTimeout (an idle worker's: about 0.8 s, 1.6 s, or the sendWatch's 1 to
// 2 s) meet a conn whose clock is already past the bound. The first sample of a
// stall is a baseline, not evidence: a peer that reads is not closed on it, as
// a closing conn's is not (noteClosingAcked). A peer that reads nothing is
// closed at the sample after, one bound later.
func TestDetachedSparseSweepsGiveAReadingPeerABound888(t *testing.T) {
	for _, reads := range []bool{true, false} {
		name := "peer-reads"
		if !reads {
			name = "peer-reads-nothing"
		}
		t.Run(name, func(t *testing.T) {
			s := newStall888(t, stall888Bound)
			s.cs.lastActivity = time.Now().Add(-time.Hour).UnixNano()
			fillSocket888(t, s.cs.fd)
			gap := stall888Bound * 3 / 2
			buf := make([]byte, 4096)
			closedAt := -1
			for i := 0; i < 4 && closedAt < 0; i++ {
				s.sweepNow()
				if s.cs.closing {
					closedAt = i
					break
				}
				// The peer reads between the sweeps, in small steps: its
				// acknowledged count grows the whole time.
				for el := time.Duration(0); el < gap; el += 20 * time.Millisecond {
					if reads {
						_ = s.peer.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
						_, _ = s.peer.Read(buf)
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			switch {
			case reads && closedAt >= 0:
				t.Fatalf("a peer that read all along was closed at sweep %d (sweeps %v apart, WriteTimeout %v)", closedAt, gap, stall888Bound)
			case !reads && closedAt < 0:
				t.Fatalf("a peer that read nothing was not closed in 4 sweeps (sweeps %v apart, WriteTimeout %v)", gap, stall888Bound)
			case !reads && closedAt < 2:
				t.Fatalf("closed at sweep %d: the first sample after the bound is a baseline, the close is due at the next", closedAt)
			}
		})
	}
}

// TestStallLookRateLimit888: the look at the detached conns takes place at most
// once per a sixteenth of the bound, within 25 ms to 250 ms, never without a
// WriteTimeout, and a clock that stepped back looks at once.
func TestStallLookRateLimit888(t *testing.T) {
	ms := func(n int64) int64 { return n * int64(time.Millisecond) }
	for _, c := range []struct {
		wt    time.Duration
		floor int64 // ns
	}{
		{300 * time.Millisecond, ms(25)},
		{time.Second, int64(time.Second) / 16},
		{60 * time.Second, ms(250)},
	} {
		w := &Worker{}
		w.cfg.WriteTimeout = c.wt
		base := ms(1_000_000)
		if !w.stallLookDue(base) {
			t.Fatalf("WriteTimeout %v: the first sweep did not look", c.wt)
		}
		if w.stallLookDue(base + c.floor - 1) {
			t.Errorf("WriteTimeout %v: looked again 1 ns before the floor of %v", c.wt, time.Duration(c.floor))
		}
		if !w.stallLookDue(base + c.floor) {
			t.Errorf("WriteTimeout %v: did not look at the floor of %v", c.wt, time.Duration(c.floor))
		}
		if !w.stallLookDue(base - ms(1000)) {
			t.Errorf("WriteTimeout %v: a clock that stepped back did not look", c.wt)
		}
	}
	for _, wt := range []time.Duration{0, -1} {
		w := &Worker{}
		w.cfg.WriteTimeout = wt
		if w.stallLookDue(ms(5_000_000)) {
			t.Errorf("WriteTimeout %v: looked", wt)
		}
	}
}

// TestStalledCloseSkipsTheClosingDrain888: the peer has just been shown to take
// nothing for WriteTimeout, so the drain that closeConn gives a conn with a
// SEND in flight (closingDrainBound, 5 s or more) is spent at once: the next
// sweep may reap it. The closing sample that comes first (noteClosingAcked)
// must not give it a fresh drain while the peer stays silent, because the
// stall's sample is its baseline; a peer that reads in the meantime restamps
// the clock.
func TestStalledCloseSkipsTheClosingDrain888(t *testing.T) {
	s := newStall888(t, stall888Bound)
	s.cs.lastActivity = time.Now().UnixNano()
	fillSocket888(t, s.cs.fd)
	if _, closing := s.sweepUntilClosing(5*time.Second, nil); !closing {
		t.Fatal("not closed")
	}
	bound := s.w.closingDrainBound()
	spent := func(when string) {
		t.Helper()
		if age := time.Now().UnixNano() - s.cs.lastActivity; age <= bound {
			t.Fatalf("%s: the closing drain's clock is %v old, the bound is %v: the stalled conn would hold its descriptor and queue for another bound",
				when, time.Duration(age), time.Duration(bound))
		}
	}
	spent("after the stall close")
	// The closing sample, a quarter of the bound later, with the peer silent.
	later := time.Now().UnixNano() + bound/4 + int64(time.Millisecond)
	s.w.noteClosingAcked(s.cs, later)
	spent("after a closing sample of a silent peer")
	// The peer reads: its acknowledged count moves, and the drain goes on.
	buf := make([]byte, 64<<10)
	for i := 0; i < 5; i++ {
		_ = s.peer.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		_, _ = s.peer.Read(buf)
	}
	time.Sleep(50 * time.Millisecond)
	s.w.noteClosingAcked(s.cs, later+int64(time.Second))
	if s.cs.lastActivity != later+int64(time.Second) {
		t.Fatalf("a peer that read after the stall close did not restamp the drain clock (lastActivity %d, want %d)", s.cs.lastActivity, later+int64(time.Second))
	}
}
