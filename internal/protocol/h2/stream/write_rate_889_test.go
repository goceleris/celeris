package stream

import (
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
	s.BufferOutbound(make([]byte, 1000), true)
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
