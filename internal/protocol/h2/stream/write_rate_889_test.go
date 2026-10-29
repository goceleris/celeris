package stream

import (
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// The refinements of the minimum rate that keep it from cutting a slow reader that
// is moving (celeris#889, D2, review r7): the carry-over of surplus releases and the
// seed of a stream's first window (F1), the connection's rate and the bound on a
// stream that takes less than its share (F6), the record judged is the one found due
// (F2), deadlines that cannot overflow (F3), the floor that follows what the stream
// holds (F4).

// window889 runs one window's end for the manager at the instant after n windows
// from the start, returning the streams expired.
func windowEnd889(m *Manager, start int64, n int) []uint32 {
	return m.ExpiredWrites(start+int64(n)*(int64(wt889)+int64(time.Millisecond)), make([]uint32, 0, 32))
}

// Surplus released in one window carries to the next ones, up to writeBankWindows
// windows' worth: a client that updates every few windows survives, and one big
// release does not hold a stream longer than the bank.
func TestWriteSurplusCarriesOverUpToTheBank889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	s := stalled889(m, 1, 8<<20)
	start := time.Now().UnixNano()
	s.SetWindowSize(3 << 20) // 3 MiB at once, in window 1
	m.FlushOutbound(s, discard889)
	// Window 1 is held on the release, the bank is capped at writeBankWindows floors:
	// it covers writeBankWindows more windows, and the one after is the verdict.
	for w := 1; w <= 1+writeBankWindows; w++ {
		if out := windowEnd889(m, start, w); len(out) != 0 {
			t.Fatalf("window %d: expired %v, want it held by the carried surplus", w, out)
		}
	}
	if out := windowEnd889(m, start, 2+writeBankWindows); len(out) != 1 {
		t.Fatalf("window %d: expired %v, want the stream (the bank is spent)", 2+writeBankWindows, out)
	}
}

// A release of twice the floor every other window (or four times every fourth) is a
// rate of one floor per window: survives for ever; half of that is reset once the
// bank is spent.
func TestWriteAverageRateIsTheGuarantee889(t *testing.T) {
	for _, tc := range []struct {
		name    string
		every   int // a release every `every` windows
		release int
		holds   bool
	}{
		{"floor-on-average", 2, 2 * 65536, true},
		{"floor-on-average-sparse", 4, 4 * 65536, true},
		{"half-the-floor", 2, 65536, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, m, _ := wdSetup889(t)
			m.initWin.Store(1 << 20) // the stream stalls with a megabyte in flight: a full seed
			s := stalled889(m, 1, 64<<20)
			start := time.Now().UnixNano()
			expiredAt := 0
			for w := 1; w <= 200 && expiredAt == 0; w++ {
				if (w-1)%tc.every == tc.every-1 {
					s.SetWindowSize(int32(tc.release))
					m.FlushOutbound(s, discard889)
				}
				if out := windowEnd889(m, start, w); len(out) != 0 {
					expiredAt = w
				}
			}
			if tc.holds && expiredAt != 0 {
				t.Fatalf("a stream that releases the floor on average was reset in window %d", expiredAt)
			}
			if !tc.holds && expiredAt == 0 {
				t.Fatal("a stream that releases less than the floor on average was never reset")
			}
		})
	}
}

// A stream's first window is covered by the bytes it had in flight when it stalled
// (the peer's initial window less what is left of it), up to the bank: a client with
// a large window and a slow first update is not reset at the first judgement.
func TestWriteSeedCoversTheFirstWindow889(t *testing.T) {
	for _, tc := range []struct {
		name     string
		initWin  uint32
		windows  int // windows held with nothing released
		wantHeld int
	}{
		{"1MiB-window-4-floors-of-cover", 1 << 20, 0, writeBankWindows},
		{"default-window-65535-is-not-a-floor", 65535, 0, 0},
		{"zero-in-flight", 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, m, _ := wdSetup889(t)
			m.initWin.Store(tc.initWin)
			stalled889(m, 1, 8<<20) // window 0: the stream sent initWin bytes
			start := time.Now().UnixNano()
			held := 0
			for w := 1; w <= 10; w++ {
				if out := windowEnd889(m, start, w); len(out) != 0 {
					break
				}
				held++
			}
			if held != tc.wantHeld {
				t.Fatalf("held %d windows with nothing released, want %d", held, tc.wantHeld)
			}
		})
	}
}

// A WINDOW_UPDATE(1) dribbler is reset at the first judgement that the seed does not
// cover: with a 64 KiB window the seed (65535) and a few bytes are over the floor
// once, and the second window is the verdict.
func TestWriteDribblerIsResetWithinTwoWindowsOfADefaultWindow889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	m.initWin.Store(65535)
	s := stalled889(m, 1, 4<<20)
	start := time.Now().UnixNano()
	n := 0
	for w := 1; w <= 5 && n == 0; w++ {
		for i := 0; i < 10; i++ { // 10 one-byte grants a window
			s.SetWindowSize(1)
			m.FlushOutbound(s, discard889)
		}
		if out := windowEnd889(m, start, w); len(out) != 0 {
			n = w
		}
	}
	if n == 0 || n > 2 {
		t.Fatalf("the dribbler was reset in window %d, want 1 or 2", n)
	}
}

// On a connection that releases above the floor a stream that takes less than its
// share is held for writeIdleWindows windows and reset at the next; one that takes
// its share is held for as long as the connection moves; when the connection itself
// is below the floor the first window is the verdict.
func TestWriteIdleStreamOnAHealthyConnIsBounded889(t *testing.T) {
	setup := func(t *testing.T) (*Manager, *Stream, *Stream) {
		_, m, _ := wdSetup889(t)
		a := stalled889(m, 1, 32<<20)
		b := stalled889(m, 3, 32<<20)
		return m, a, b
	}
	t.Run("zero-release-is-held-K-windows", func(t *testing.T) {
		m, a, _ := setup(t)
		start := time.Now().UnixNano()
		for w := 1; w <= writeIdleWindows+1; w++ {
			a.SetWindowSize(70000)
			m.FlushOutbound(a, discard889) // the sibling moves the connection above the floor
			out := windowEnd889(m, start, w)
			if w <= writeIdleWindows && len(out) != 0 {
				t.Fatalf("window %d: expired %v, want stream 3 held (it may be queued behind its sibling)", w, out)
			}
			if w == writeIdleWindows+1 && (len(out) != 1 || out[0] != 3) {
				t.Fatalf("window %d: expired %v, want stream 3", w, out)
			}
		}
	})
	t.Run("a-dribble-is-no-share", func(t *testing.T) {
		m, a, b := setup(t)
		start := time.Now().UnixNano()
		for w := 1; w <= writeIdleWindows+1; w++ {
			a.SetWindowSize(70000)
			m.FlushOutbound(a, discard889)
			for i := 0; i < 10; i++ {
				b.SetWindowSize(1)
				m.FlushOutbound(b, discard889)
			}
			out := windowEnd889(m, start, w)
			if (w <= writeIdleWindows) != (len(out) == 0) {
				t.Fatalf("window %d: expired %v", w, out)
			}
		}
	})
	t.Run("its-share-holds-it-for-ever", func(t *testing.T) {
		m, a, b := setup(t)
		start := time.Now().UnixNano()
		for w := 1; w <= 30; w++ {
			a.SetWindowSize(40000)
			m.FlushOutbound(a, discard889)
			b.SetWindowSize(40000) // below the floor, above half of floor/2
			m.FlushOutbound(b, discard889)
			if out := windowEnd889(m, start, w); len(out) != 0 {
				t.Fatalf("window %d: expired %v although both take 40000 a window and the connection 80000", w, out)
			}
		}
	})
	t.Run("a-slow-connection-is-the-verdict-at-once", func(t *testing.T) {
		m, a, b := setup(t)
		start := time.Now().UnixNano()
		a.SetWindowSize(20000)
		m.FlushOutbound(a, discard889)
		b.SetWindowSize(20000)
		m.FlushOutbound(b, discard889) // 40000 in all: below the floor
		if out := windowEnd889(m, start, 1); len(out) != 2 {
			t.Fatalf("expired %v, want both: the connection itself did not release its floor", out)
		}
	})
}

// The record a verdict is for is the one found due: one that ended and began again
// while the windows were read is a new window.
func TestExpiredWritesJudgesTheRecordItFoundDue889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	s := stalled889(m, 1, 100000)
	m.stallMu.Lock()
	m.stalls[1].armedAt -= int64(2 * wt889)
	m.stallMu.Unlock()

	m.mu.Lock() // hold the read of the streams' windows
	done := make(chan []uint32)
	go func() { done <- m.ExpiredWrites(time.Now().UnixNano(), make([]uint32, 0, 32)) }()
	time.Sleep(50 * time.Millisecond)
	// Meanwhile a pool goroutine's flush takes the whole buffer and it buffers a new tail.
	s.SetWindowSize(1 << 20)
	m.FlushOutbound(s, discard889)
	s.SetWindowSize(0)
	s.BufferOutbound(make([]byte, 100000), true) // not a tail: the tail grace would hold it
	m.mu.Unlock()
	if got := <-done; len(got) != 0 {
		t.Fatalf("expired %v: the record judged was a new one whose window began microseconds ago", got)
	}
}

// A stream whose buffer went out whole between the verdict and the reset is not
// reset (its END_STREAM is out).
func TestExpireWriteSkipsAStreamWithNothingHeld889(t *testing.T) {
	p, m, rec := wdSetup889(t)
	s := stalled889(m, 1, 5000)
	s.SetWindowSize(1 << 20)
	m.FlushOutbound(s, discard889)
	p.ExpireWrite(1)
	if rec.count() != 0 {
		t.Fatalf("reset a stream with nothing held: %+v", rec.rst)
	}
}

// A deadline of math.MaxInt64 (the common way to write "never") must not overflow:
// the window of a stalled stream and the request deadline are far in the future.
func TestHugeTimeoutsDoNotOverflow889(t *testing.T) {
	const never = time.Duration(1<<63 - 1)
	t.Run("write", func(t *testing.T) {
		_, m, _ := wdSetup889(t)
		m.SetWriteTimeout(never)
		stalled889(m, 1, 5000)
		if h := m.NextWriteDeadline(); h <= time.Now().UnixNano() {
			t.Fatalf("hint %d is not in the future", h)
		}
		if out := m.ExpiredWrites(time.Now().UnixNano()+int64(time.Hour), make([]uint32, 0, 4)); len(out) != 0 {
			t.Fatalf("expired %v an hour in", out)
		}
	})
	t.Run("request", func(t *testing.T) {
		m := NewManager()
		m.SetRequestTimeout(never)
		s := m.CreateStream(1)
		s.SetState(StateOpen)
		m.ArmRequest(s, time.Now().UnixNano())
		if h := m.NextRequestDeadline(); h <= time.Now().UnixNano() {
			t.Fatalf("request deadline hint %d is not in the future", h)
		}
		if out := m.ExpiredRequests(time.Now().UnixNano()+int64(time.Hour), make([]uint32, 0, 4)); len(out) != 0 {
			t.Fatalf("expired %v an hour in", out)
		}
	})
	t.Run("header-block", func(t *testing.T) {
		p, _, _ := wdSetup889(t)
		p.SetHeaderBlockTimeout(never)
		if p.hdrTimeout <= 0 || p.hdrTimeout > maxTimeout {
			t.Fatalf("header block timeout %v", p.hdrTimeout)
		}
	})
}

// The floor of a later window follows what the stream holds then, not what it held
// when its first window began.
func TestWriteFloorFollowsWhatTheStreamHolds889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	s := stalled889(m, 1, 100000) // held 100000: floor 65536
	start := time.Now().UnixNano()
	s.SetWindowSize(70000) // 70000 released, 30000 left
	m.FlushOutbound(s, discard889)
	if out := windowEnd889(m, start, 1); len(out) != 0 {
		t.Fatalf("window 1: expired %v", out)
	}
	s.BufferOutbound(make([]byte, 100000), true) // 130000 held now; the window began with 30000
	s.SetWindowSize(30000)                       // floor of this window is min(64 KiB, 30000) = 30000
	m.FlushOutbound(s, discard889)
	if out := windowEnd889(m, start, 2); len(out) != 0 {
		t.Fatalf("window 2: expired %v although it released the 30000 it held when the window began", out)
	}
}

// The tail of a response (what is held is under one floor) is not judged on
// "all of it in one window": the grant for the last bytes can land just after the
// window (r8 F-A). Half of it released is a grace window, inside the idle bound;
// nothing is the verdict, and a tail that goes on missing half of itself is reset
// after writeIdleWindows such windows.
func TestWriteTailGraceIsBounded889(t *testing.T) {
	// Nothing released is not the verdict at the first window (r9 F-A2: a connection
	// that holds under one floor is draining its backlog, and a window can pass
	// with nothing going out); it is the verdict after writeIdleWindows of them.
	t.Run("nothing-released-is-held-for-the-idle-windows", func(t *testing.T) {
		_, m, _ := wdSetup889(t)
		stalled889(m, 1, 1000)
		if w := firstReset889(m, 1, time.Now().UnixNano(), 10, nil); w != writeIdleWindows+1 {
			t.Fatalf("reset in window %d, want %d (held for %d)", w, writeIdleWindows+1, writeIdleWindows)
		}
	})
	t.Run("half-every-window-is-bounded", func(t *testing.T) {
		_, m, _ := wdSetup889(t)
		s := stalled889(m, 1, 1000)
		start := time.Now().UnixNano()
		for w := 1; w <= writeIdleWindows+1; w++ {
			s.SetWindowSize(500) // half of the 1000 held at the window's start...
			m.FlushOutbound(s, discard889)
			s.BufferOutbound(make([]byte, 500), true) // ...and the handler adds as much
			out := windowEnd889(m, start, w)
			if (w <= writeIdleWindows) != (len(out) == 0) {
				t.Fatalf("window %d: expired %v (grace for %d windows)", w, out, writeIdleWindows)
			}
		}
	})
}

// A tail held by the whole connection (several streams each waiting for a few
// bytes, the connection's floor made of those tails) is held without proof that it
// moves: the end of a transfer is on the wire, and the drain of its backlog can take
// more than a window (r9 F-A2; r8 judged by half of the connection's floor, which a
// slow drain does not reach). Bounded by the idle windows, whatever went out.
func TestWriteConnTailGrace889(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release int
	}{
		{"half-of-the-conn-floor-went-out", 100},
		{"under-half", 99},
		{"nothing-went-out", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, m, _ := wdSetup889(t)
			start := time.Now().UnixNano()
			for i := 0; i < 3; i++ {
				stalled889(m, uint32(1+2*i), 100)
			}
			c := m.CreateStream(99)
			c.SetState(StateOpen)
			c.SetWindowSize(1 << 20)
			if tc.release > 0 {
				m.ReserveSendWindow(c, int32(tc.release)) // a sibling's direct send: the conn releases
			}
			if w := firstReset889(m, 5, start, 10, nil); w != writeIdleWindows+1 {
				t.Fatalf("stream 5 reset in window %d, want %d (held for the %d idle windows)", w, writeIdleWindows+1, writeIdleWindows)
			}
		})
	}
}

// firstReset889 runs windows and reports the one in which stream id is expired
// (0 if it is not in limit windows). before(w) acts at the start of window w.
func firstReset889(m *Manager, id uint32, start int64, limit int, before func(w int)) int {
	for w := 1; w <= limit; w++ {
		if before != nil {
			before(w)
		}
		for _, e := range windowEnd889(m, start, w) {
			if e == id {
				return w
			}
		}
	}
	return 0
}

// The true bounds of an unread stream (r8 F-C, derived from the code): the bytes it
// had in flight when it stalled (the peer's initial window less what was left of its
// own, up to four floors) are spent first, a floor a window; then, alone, the verdict;
// with a sibling that keeps the connection above the floor, writeIdleWindows more.
// So a stream the client never grants is reset in window 1+k alone and 1+k+3 with a
// moving sibling, k = min(4, floor(in flight / 64 KiB)): 5 and 8 for a window of
// 256 KiB or more (5 and 8 x WriteTimeout), against 1 and 4 for the default window.
func TestWriteUnreadStreamBounds889(t *testing.T) {
	for _, tc := range []struct {
		initWin    uint32
		alone, sib int
	}{
		{65535, 1, 4},
		{128 << 10, 3, 6},
		{192 << 10, 4, 7},
		{256 << 10, 5, 8},
		{1 << 20, 5, 8},
		{1<<31 - 1, 5, 8},
	} {
		t.Run(strconv.Itoa(int(tc.initWin>>10))+"KiB", func(t *testing.T) {
			_, m, _ := wdSetup889(t)
			m.initWin.Store(tc.initWin)
			stalled889(m, 1, 8<<20)
			if w := firstReset889(m, 1, time.Now().UnixNano(), 20, nil); w != tc.alone {
				t.Fatalf("alone: reset in window %d, want %d", w, tc.alone)
			}
			_, m2, _ := wdSetup889(t)
			m2.initWin.Store(tc.initWin)
			b := stalled889(m2, 1, 8<<20)
			a := m2.CreateStream(3)
			a.SetState(StateOpen)
			a.SetWindowSize(0)
			a.BufferOutbound(make([]byte, 64<<20), true)
			_ = b
			w := firstReset889(m2, 1, time.Now().UnixNano(), 20, func(int) { a.SetWindowSize(70000); m2.FlushOutbound(a, discard889) })
			if w != tc.sib {
				t.Fatalf("with a sibling that moves the connection: reset in window %d, want %d", w, tc.sib)
			}
		})
	}
}

// A one-byte dribbler: reset in window 2 with a default window (the seed and a few
// bytes cover window 1), in window 5 with a window of 256 KiB or more (four floors
// of seed, spent a window each).
func TestWriteDribblerBounds889(t *testing.T) {
	for _, tc := range []struct {
		initWin uint32
		want    int
	}{{65535, 2}, {128 << 10, 3}, {256 << 10, 5}, {1 << 30, 5}} {
		t.Run(strconv.Itoa(int(tc.initWin>>10))+"KiB", func(t *testing.T) {
			_, m, _ := wdSetup889(t)
			m.initWin.Store(tc.initWin)
			s := stalled889(m, 1, 8<<20)
			w := firstReset889(m, 1, time.Now().UnixNano(), 20, func(int) {
				for i := 0; i < 10; i++ {
					s.SetWindowSize(1)
					m.FlushOutbound(s, discard889)
				}
			})
			if w != tc.want {
				t.Fatalf("reset in window %d, want %d", w, tc.want)
			}
		})
	}
}

// The idle count restarts when the stream takes its share, its floor, or is
// starved by the connection window: a stream that alternates is not reset early.
func TestWriteIdleCountRestarts889(t *testing.T) {
	setup := func(t *testing.T) (*Manager, *Stream, *Stream) {
		_, m, _ := wdSetup889(t)
		b := stalled889(m, 1, 64<<20)
		a := stalled889(m, 3, 64<<20)
		return m, a, b
	}
	move := func(m *Manager, a *Stream) { a.SetWindowSize(70000); m.FlushOutbound(a, discard889) }
	t.Run("share", func(t *testing.T) {
		m, a, b := setup(t)
		got := firstReset889(m, 1, time.Now().UnixNano(), 20, func(w int) {
			move(m, a)
			if w == 2 { // one window in which it takes its share
				b.SetWindowSize(20000)
				m.FlushOutbound(b, discard889)
			}
		})
		if got != 2+writeIdleWindows+1 {
			t.Fatalf("reset in window %d, want %d (idle 1, a share window, then %d idle windows held)", got, 2+writeIdleWindows+1, writeIdleWindows)
		}
	})
	t.Run("floor", func(t *testing.T) {
		m, a, b := setup(t)
		got := firstReset889(m, 1, time.Now().UnixNano(), 20, func(w int) {
			move(m, a)
			if w == 2 {
				b.SetWindowSize(70000)
				m.FlushOutbound(b, discard889)
			}
		})
		if got != 2+writeIdleWindows+1 {
			t.Fatalf("reset in window %d, want %d", got, 2+writeIdleWindows+1)
		}
	})
	t.Run("starved", func(t *testing.T) {
		m, a, b := setup(t)
		got := firstReset889(m, 1, time.Now().UnixNano(), 20, func(w int) {
			if w == 3 { // the conn window is shut and b's own has room: starved; a sibling takes the grant
				atomic.StoreInt32(&m.connectionWindow, 0)
				b.SetWindowSize(1 << 20)
				a.SetWindowSize(70000)
				m.UpdateConnectionWindow(70000)
				m.FlushOutbound(a, discard889)
				return
			}
			b.SetWindowSize(0)
			m.UpdateConnectionWindow(70000)
			move(m, a)
		})
		if got != 3+writeIdleWindows+1 {
			t.Fatalf("reset in window %d, want %d (a starved window restarts the count)", got, 3+writeIdleWindows+1)
		}
	})
}

// The share is half the connection's floor divided among the streams stalled.
func TestWriteShareScalesWithTheStreamsAndHalves889(t *testing.T) {
	run := func(t *testing.T, n, release int) int {
		_, m, _ := wdSetup889(t)
		b := stalled889(m, 1, 64<<20)
		a := stalled889(m, 3, 64<<20)
		for i := 2; i < n; i++ {
			stalled889(m, uint32(1+2*i), 64<<20)
		}
		return firstReset889(m, 1, time.Now().UnixNano(), 12, func(int) {
			a.SetWindowSize(70000)
			m.FlushOutbound(a, discard889)
			b.SetWindowSize(int32(release))
			m.FlushOutbound(b, discard889)
		})
	}
	// N = 2: the share is 65536 / (2 x 2) = 16384.
	if w := run(t, 2, 20000); w != 0 {
		t.Fatalf("N=2, 20000 a window (above the share): reset in window %d", w)
	}
	if w := run(t, 2, 10000); w != 1+writeIdleWindows {
		t.Fatalf("N=2, 10000 a window (under the share): reset in window %d, want %d", w, 1+writeIdleWindows)
	}
	// N = 10: the share is 65536 / 20 = 3276.
	if w := run(t, 10, 4000); w != 0 {
		t.Fatalf("N=10, 4000 a window (above the share): reset in window %d", w)
	}
	if w := run(t, 10, 2000); w != 1+writeIdleWindows {
		t.Fatalf("N=10, 2000 a window (under the share): reset in window %d, want %d", w, 1+writeIdleWindows)
	}
}

// The seed is the bytes in flight, the initial window less what is left of it.
func TestWriteSeedIsWhatIsInFlight889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	m.initWin.Store(1 << 20)
	s := m.CreateStream(1)
	s.SetState(StateOpen)
	s.SetWindowSize(1<<20 - 100000) // 100000 bytes sent, not granted back
	s.BufferOutbound(make([]byte, 8<<20), true)
	// 100000 covers window 1 (above the floor) and not window 2.
	if w := firstReset889(m, 1, time.Now().UnixNano(), 10, nil); w != 2 {
		t.Fatalf("reset in window %d, want 2: the seed is what is in flight, not the initial window", w)
	}
}

// A connection whose floor is its few held bytes is "moving" when it releases them.
func TestWriteConnFloorIsWhatTheConnHolds889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	stalled889(m, 1, 100)
	stalled889(m, 3, 100)
	c := m.CreateStream(99)
	c.SetState(StateOpen)
	c.SetWindowSize(1 << 20)
	m.ReserveSendWindow(c, 400) // a sibling releases 400 bytes: more than the connection held
	if out := m.ExpiredWrites(dueAt889(), make([]uint32, 0, 8)); len(out) != 0 {
		t.Fatalf("expired %v: the connection released more than it held (its floor is what it holds, not 64 KiB)", out)
	}
}

// The bank does not survive a window held on the share: a stream below its floor
// on a connection that does not move is the verdict.
func TestWriteBankIsSpentByAShareWindow889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	b := stalled889(m, 1, 64<<20)
	a := stalled889(m, 3, 64<<20)
	start := time.Now().UnixNano()
	// Window 1: b releases 95536 (above the floor: 30000 banked) and a moves the conn.
	b.SetWindowSize(95536)
	m.FlushOutbound(b, discard889)
	a.SetWindowSize(70000)
	m.FlushOutbound(a, discard889)
	if out := windowEnd889(m, start, 1); len(out) != 0 {
		t.Fatalf("window 1: %v", out)
	}
	// Window 2: b takes 20000 (under the floor, over its share) while a moves the conn.
	b.SetWindowSize(20000)
	m.FlushOutbound(b, discard889)
	a.SetWindowSize(70000)
	m.FlushOutbound(a, discard889)
	if out := windowEnd889(m, start, 2); len(out) != 0 {
		t.Fatalf("window 2: %v", out)
	}
	// Window 3: b takes 40000 and nothing else moves: 40000 is under the floor and the
	// connection is under its floor. The 30000 banked in window 1 do not make 70000.
	b.SetWindowSize(40000)
	m.FlushOutbound(b, discard889)
	reset := false
	for _, id := range windowEnd889(m, start, 3) {
		reset = reset || id == 1
	}
	if !reset {
		t.Fatal("window 3: stream 1 held; the bank of window 1 survived a window held on the share")
	}
}

// The stream's own tail is judged by half even when the connection holds much more
// (a sibling with a megabyte waiting) and does not move: the conn clause does not
// apply, the stream clause does.
func TestWriteStreamTailGraceBesideABigSibling889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	stalled889(m, 1, 1<<20) // the sibling: nothing released, a verdict at once
	tail := stalled889(m, 3, 10000)
	tail.SetWindowSize(6000) // 6000 of its 10000, under the connection's floor
	m.FlushOutbound(tail, discard889)
	got := map[uint32]bool{}
	for _, id := range m.ExpiredWrites(dueAt889(), make([]uint32, 0, 8)) {
		got[id] = true
	}
	if got[3] {
		t.Fatal("the tail that went out 60% was reset")
	}
	if !got[1] {
		t.Fatal("the sibling that released nothing was held")
	}
}

// An unread stream whose unsent rest is under one floor is let go as fast as a large
// one (r9 F-H): the floor is what it holds, so the bytes in flight (the seed, up to 256
// KiB) were spent at a few bytes per window, 65,536 windows for the default initial
// window and 262,145 for 256 KiB or more, and the connection was exempt from
// ReadTimeout and IdleTimeout all that time. A window that passes on the carried bank
// pays a whole floor: the bytes in flight are gone in 1+k windows (k as in
// TestWriteUnreadStreamBounds889), and the connection holding under one floor gets the
// tail's three idle windows on top: window 5 (default window), 6 (128 KiB), 8 (256 KiB
// or more).
func TestWriteUnreadSmallRemainderBounds889(t *testing.T) {
	for _, held := range []int{1, 100, 10000, 60000} {
		for _, tc := range []struct {
			initWin uint32
			want    int
		}{{65535, 5}, {128 << 10, 6}, {1 << 20, 8}} {
			t.Run(strconv.Itoa(held)+"B/"+strconv.Itoa(int(tc.initWin>>10))+"KiB", func(t *testing.T) {
				_, m, _ := wdSetup889(t)
				m.initWin.Store(tc.initWin)
				stalled889(m, 1, held)
				if w := firstReset889(m, 1, time.Now().UnixNano(), 40, nil); w != tc.want {
					t.Fatalf("reset in window %d, want %d (0 means not within 40)", w, tc.want)
				}
			})
		}
	}
}

// The carry-over of a streamed response survives a small window: a stream that
// released 312 KiB of 320 KiB in one window and then buffers a megabyte keeps its
// bank (r9, why the bank is not capped by the floor of the window judged).
func TestWriteBankSurvivesASmallRenewal889(t *testing.T) {
	for _, order := range []string{"buffer-after-renewal", "buffer-before-renewal"} {
		t.Run(order, func(t *testing.T) {
			_, m, _ := wdSetup889(t)
			s := stalled889(m, 1, 320<<10)
			start := time.Now().UnixNano()
			s.SetWindowSize(312 << 10)
			m.FlushOutbound(s, discard889)
			if order == "buffer-before-renewal" {
				s.BufferOutbound(make([]byte, 1<<20), true)
			}
			got := firstReset889(m, 1, start, 50, func(w int) {
				if w == 2 && order == "buffer-after-renewal" {
					s.BufferOutbound(make([]byte, 1<<20), true)
				}
			})
			if got < 5 {
				t.Fatalf("reset in window %d, want at least 5: the 256 KiB carried over must not be cut by a small window", got)
			}
		})
	}
}

// A dribbler with a small rest does not live on its bank either (r9 F-H): one byte a
// window released is not a floor, so each window pays a whole one.
func TestWriteDribblerWithASmallRestSpendsItsBank889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	m.initWin.Store(1 << 20) // 256 KiB of bank
	s := stalled889(m, 1, 1000)
	got := firstReset889(m, 1, time.Now().UnixNano(), 40, func(int) {
		s.SetWindowSize(1)
		m.FlushOutbound(s, discard889)
		s.BufferOutbound([]byte{0}, true)
	})
	if got != 8 {
		t.Fatalf("reset in window %d, want 8 (0 means not within 40: the bank was spent at the rest, not at a floor, a window)", got)
	}
}

// What a window pays beyond the bank is not owed later: a stream that spent a bank
// of less than a floor on a window it released nothing in starts the next from zero,
// not from a debt (r9, the bank is clamped at 0). A stream that then releases all of
// its small rest every window is held for as long as it does.
func TestWriteBankIsNotADebt889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	m.initWin.Store(20000) // a bank of 20000, less than a floor
	s := stalled889(m, 1, 1000)
	start := time.Now().UnixNano()
	got := firstReset889(m, 1, start, 12, func(w int) {
		if w > 1 { // window 1 releases nothing and pays a whole floor for it
			s.SetWindowSize(1000)
			m.FlushOutbound(s, discard889)
			s.BufferOutbound(make([]byte, 1000), true)
		}
	})
	if got != 0 {
		t.Fatalf("reset in window %d: a stream that releases all it holds every window is held", got)
	}
}

// On a connection that holds less than a floor, a stream that takes its share while
// the connection moves is held and its idle count cleared (the share clause, not the
// tail's, which counts every such window): the connection's floor is what it holds
// (r8 p8, still true with the r9 tail grace).
func TestWriteSmallConnShareHoldClearsIdle889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	a := stalled889(m, 1, 9000)
	b := stalled889(m, 3, 1000)
	c := m.CreateStream(99)
	c.SetState(StateOpen)
	c.SetWindowSize(1 << 30)
	start := time.Now().UnixNano()
	got := firstReset889(m, 1, start, 12, func(int) {
		a.SetWindowSize(3000) // more than its share (10000 / (2 x 2)), less than half of what it holds
		m.FlushOutbound(a, discard889)
		a.BufferOutbound(make([]byte, 3000), true)
		b.SetWindowSize(1000) // b takes all it holds: its floor
		m.FlushOutbound(b, discard889)
		b.BufferOutbound(make([]byte, 1000), true)
		m.ReserveSendWindow(c, 8000) // the connection moves above what it holds
	})
	if got != 0 {
		t.Fatalf("stream 1 reset in window %d although it took its share of a moving connection every window", got)
	}
}
