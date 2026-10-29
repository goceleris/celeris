package stream

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// The write deadline's books and verdicts (celeris#889, D2): the records, the
// floor, the conn credit, the removal paths, the reset.

// rstRecorder889 is a connection writer that implements Resetter and keeps what it
// was asked to reset.
type rstRecorder889 struct {
	*testResponseWriter
	mu  sync.Mutex
	rst []rstRecord889
}

type rstRecord889 struct {
	id   uint32
	code http2.ErrCode
}

func (r *rstRecorder889) ResetStream(s *Stream, code http2.ErrCode) {
	r.mu.Lock()
	r.rst = append(r.rst, rstRecord889{s.ID, code})
	r.mu.Unlock()
	s.Cancel()
}

func (r *rstRecorder889) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rst)
}

const wt889 = time.Second

// wdSetup889 is a processor with a write deadline of wt889, an open connection
// window and a recorder for the resets.
func wdSetup889(t testing.TB) (*Processor, *Manager, *rstRecorder889) {
	t.Helper()
	rec := &rstRecorder889{testResponseWriter: newTestResponseWriter()}
	p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), newTestFrameWriter(), rec)
	m := p.GetManager()
	m.SetWriteTimeout(wt889)
	m.UpdateConnectionWindow(1 << 30)
	m.initWin.Store(0) // no seed: a stream's first window is judged on what it releases (the seed has its own tests)
	return p, m, rec
}

// stalled889 makes stream id with a zero window and n bytes buffered.
func stalled889(m *Manager, id uint32, n int) *Stream {
	s := m.CreateStream(id)
	s.SetState(StateOpen)
	s.SetWindowSize(0)
	s.BufferOutbound(make([]byte, n), true)
	return s
}

func discard889(uint32, bool, []byte) {}

func dueAt889() int64 { return time.Now().UnixNano() + int64(wt889) + int64(time.Millisecond) }

// A record is made when a buffer fills and only with a write deadline, and the hint
// is the end of the first window; sending that fits the window makes none.
func TestStallIsArmedByABufferOnly889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	// Fits the window: sent, nothing buffered, no record, no hint.
	s := m.CreateStream(1)
	s.SetState(StateOpen)
	if _, _, _, err := m.StreamWrite(s, s.Gen(), make([]byte, 1000), discard889); err != nil {
		t.Fatal(err)
	}
	if m.stalledStreams() != 0 || m.NextWriteDeadline() != 0 {
		t.Fatalf("a write that fit the window left %d records, hint %d", m.stalledStreams(), m.NextWriteDeadline())
	}
	before := time.Now().UnixNano()
	stalled889(m, 3, 100)
	after := time.Now().UnixNano()
	if m.stalledStreams() != 1 {
		t.Fatalf("%d records after a buffer filled, want 1", m.stalledStreams())
	}
	if h := m.NextWriteDeadline(); h < before+int64(wt889) || h > after+int64(wt889) {
		t.Fatalf("hint %d, want the end of a window of %v that began between %d and %d", h, wt889, before, after)
	}
	// Without a write deadline there is no record.
	_, m2, _ := wdSetup889(t)
	m2.SetWriteTimeout(0)
	stalled889(m2, 1, 100)
	if m2.stalledStreams() != 0 || m2.NextWriteDeadline() != 0 {
		t.Fatalf("WriteTimeout off: %d records, hint %d", m2.stalledStreams(), m2.NextWriteDeadline())
	}
}

// The floor is min(64 KiB, what the stream held when the window began), and a
// window that passes it starts the next.
func TestExpiredWritesFloor889(t *testing.T) {
	for _, tc := range []struct {
		name     string
		held     int
		release  int  // bytes released in the window
		expired  bool // the stream is expired at the end of the window
		recordIn bool // it still has a record afterwards (not expired, not drained)
	}{
		{"nothing-released", 1 << 20, 0, true, true},
		{"floor-1", 1 << 20, 65535, true, true},
		{"floor", 1 << 20, 65536, false, true},
		{"above-floor", 1 << 20, 200000, false, true},
		{"small-tail-all-released", 10000, 10000, false, false}, // the buffer empties: the record ends
		// A tail (held under one floor) that went out all but a little is not the
		// verdict: half of it is a grace window (r8 F-A; it used to be reset, "all of it
		// in one window" was the floor). The grace is bounded: TestWriteTailGraceIsBounded889.
		{"small-tail-nearly-all", 10000, 9999, false, true},
		{"small-tail-half", 10000, 5000, false, true},
		{"small-tail-under-half", 10000, 4999, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, m, _ := wdSetup889(t)
			s := stalled889(m, 1, tc.held)
			if tc.release > 0 {
				s.SetWindowSize(int32(tc.release))
				m.FlushOutbound(s, discard889)
			}
			out := m.ExpiredWrites(dueAt889(), make([]uint32, 0, 8))
			if got := len(out) == 1; got != tc.expired {
				t.Fatalf("expired = %v (%v), want %v", got, out, tc.expired)
			}
			if got := m.stalledStreams() == 1; got != tc.recordIn {
				t.Fatalf("record kept = %v, want %v", got, tc.recordIn)
			}
		})
	}
}

// Not before the window ends; a stream that passed a window is judged again a
// window later, and the verdict of every window is its own (no ceiling, no credit
// carried over).
func TestExpiredWritesWindows889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	s := stalled889(m, 1, 1<<22)
	start := time.Now().UnixNano()
	if out := m.ExpiredWrites(start+int64(wt889)/2, make([]uint32, 0, 4)); len(out) != 0 {
		t.Fatalf("expired %v half a window in", out)
	}
	now := start + int64(wt889) + int64(time.Millisecond)
	for w := 0; w < 3; w++ { // three windows, each with its floor
		s.SetWindowSize(65536)
		m.FlushOutbound(s, discard889)
		if out := m.ExpiredWrites(now, make([]uint32, 0, 4)); len(out) != 0 {
			t.Fatalf("window %d: expired %v although it released its floor", w, out)
		}
		if h := m.NextWriteDeadline(); h != now+int64(wt889) {
			t.Fatalf("window %d: hint %d, want the end of the next window %d", w, h, now+int64(wt889))
		}
		now += int64(wt889) + int64(time.Millisecond)
	}
	// The fourth window releases nothing, and the credit of the third does not count.
	if out := m.ExpiredWrites(now, make([]uint32, 0, 4)); len(out) != 1 || out[0] != 1 {
		t.Fatalf("a window with no release: expired %v, want [1]", out)
	}
}

// A stream whose own window has room but whose bytes wait for the connection's is
// credited with what the connection released; one whose own window is shut is
// judged by its own releases, whatever the connection released.
func TestExpiredWritesConnStarved889(t *testing.T) {
	setup := func(t *testing.T) (*Manager, *Stream, *Stream) {
		_, m, _ := wdSetup889(t)
		atomic.StoreInt32(&m.connectionWindow, 0) // shut
		a := m.CreateStream(1)
		a.SetState(StateOpen)
		a.SetWindowSize(1 << 20)
		a.BufferOutbound(make([]byte, 1<<20), true)
		b := m.CreateStream(3)
		b.SetState(StateOpen)
		b.SetWindowSize(1 << 20)
		b.BufferOutbound(make([]byte, 1<<20), true)
		return m, a, b
	}
	t.Run("the-conn-released-the-floor", func(t *testing.T) {
		m, a, _ := setup(t)
		m.UpdateConnectionWindow(70000)
		m.FlushOutbound(a, discard889) // a sibling takes all of it: b released nothing itself
		if out := m.ExpiredWrites(dueAt889(), make([]uint32, 0, 8)); len(out) != 0 {
			t.Fatalf("expired %v although the conn released 70000 bytes in the window", out)
		}
	})
	t.Run("starved-by-the-conn-window-is-held-as-long-as-the-conn-moves", func(t *testing.T) {
		// Both streams have room in their own windows and the conn window is shut:
		// the sibling takes every grant, the other gets nothing of its own for far
		// more than writeIdleWindows windows, and is held all the time (it waits
		// for the connection, which is no fault of its own).
		m, a, _ := setup(t)
		now := dueAt889()
		for w := 0; w < 3*writeIdleWindows; w++ {
			m.UpdateConnectionWindow(70000)
			m.FlushOutbound(a, discard889)
			if out := m.ExpiredWrites(now, make([]uint32, 0, 8)); len(out) != 0 {
				t.Fatalf("window %d: expired %v while the conn released its floor", w, out)
			}
			now += int64(wt889) + int64(time.Millisecond)
		}
	})
	t.Run("the-conn-released-a-dribble", func(t *testing.T) {
		m, a, _ := setup(t)
		m.UpdateConnectionWindow(1)
		m.FlushOutbound(a, discard889)
		out := m.ExpiredWrites(dueAt889(), make([]uint32, 0, 8))
		if len(out) != 2 {
			t.Fatalf("expired %v, want both starved streams", out)
		}
	})
	t.Run("own-window-shut-is-judged-alone", func(t *testing.T) {
		// Stream 3's own window is shut while the conn released 70000 bytes, all to
		// stream 1. It is not credited with them as a starved stream is: it takes
		// nothing of its own, so it is held for writeIdleWindows windows (it may be
		// queued behind its sibling) and reset at the next.
		m, a, b := setup(t)
		b.SetWindowSize(0)
		now := dueAt889()
		for w := 0; w <= writeIdleWindows+1; w++ {
			m.UpdateConnectionWindow(70000)
			m.FlushOutbound(a, discard889)
			out := m.ExpiredWrites(now, make([]uint32, 0, 8))
			want := 0
			if w == writeIdleWindows {
				want = 1 // the (writeIdleWindows+1)th window with nothing of its own
			}
			if len(out) != want || (want == 1 && out[0] != 3) {
				t.Fatalf("window %d: expired %v, want %d (only stream 3, in window %d)", w, out, want, writeIdleWindows)
			}
			if want == 1 {
				return
			}
			now += int64(wt889) + int64(time.Millisecond)
		}
	})
	t.Run("releases-outside-a-flush-count", func(t *testing.T) {
		// A sibling that sends without buffering (a pool handler's direct send)
		// also takes the conn window: ReserveSendWindow counts it.
		m, _, _ := setup(t)
		c := m.CreateStream(5)
		c.SetState(StateOpen)
		c.SetWindowSize(1 << 20)
		m.UpdateConnectionWindow(70000)
		if g := m.ReserveSendWindow(c, 70000); g != 70000 {
			t.Fatalf("reserved %d", g)
		}
		if out := m.ExpiredWrites(dueAt889(), make([]uint32, 0, 8)); len(out) != 0 {
			t.Fatalf("expired %v although a sibling's direct send took 70000 bytes of the conn window", out)
		}
	})
}

// A batch that fills leaves the hint due and the rest for the next sweep.
func TestExpiredWritesBatch889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	for i := 0; i < 40; i++ {
		stalled889(m, uint32(1+2*i), 100)
	}
	now := dueAt889()
	out := m.ExpiredWrites(now, make([]uint32, 0, 32))
	if len(out) != 32 {
		t.Fatalf("first batch %d, want 32", len(out))
	}
	if h := m.NextWriteDeadline(); h != now {
		t.Fatalf("hint %d after a full batch, want due (%d)", h, now)
	}
	for _, id := range out {
		m.DeleteStream(id)
	}
	if out2 := m.ExpiredWrites(now, make([]uint32, 0, 32)); len(out2) != 8 {
		t.Fatalf("second batch %d, want the 8 left", len(out2))
	}
}

// A stale hint (the stall ended) costs one look, which settles it.
func TestExpiredWritesStaleHint889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	s := stalled889(m, 1, 100)
	s.SetWindowSize(1000)
	m.FlushOutbound(s, discard889) // drained: the record is gone, the hint stays
	if m.NextWriteDeadline() == 0 {
		t.Fatal("setup: no hint")
	}
	if out := m.ExpiredWrites(dueAt889(), make([]uint32, 0, 4)); len(out) != 0 {
		t.Fatalf("expired %v", out)
	}
	if m.NextWriteDeadline() != 0 {
		t.Fatalf("hint %d after the look, want 0", m.NextWriteDeadline())
	}
}

// Every path that ends a stalled stream ends its record. A record that outlived
// its stream would be a leak of the connection's life, and the net (a stream
// that is gone from the map) must not be what these rely on.
func TestStallRecordEndsWithEveryRemovalPath889(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, p *Processor, m *Manager, s *Stream)
	}{
		{"DeleteStream", func(_ *testing.T, _ *Processor, m *Manager, s *Stream) { m.DeleteStream(s.ID) }},
		{"peer-RST_STREAM", func(t *testing.T, p *Processor, _ *Manager, s *Stream) {
			if err := p.handleRSTStream(&http2.RSTStreamFrame{FrameHeader: http2.FrameHeader{StreamID: s.ID}}); err != nil {
				t.Fatal(err)
			}
		}},
		{"DiscardOutbound", func(_ *testing.T, _ *Processor, m *Manager, s *Stream) { m.DiscardOutbound(s) }},
		{"flushed-whole", func(_ *testing.T, _ *Processor, m *Manager, s *Stream) {
			s.SetWindowSize(1 << 20)
			m.FlushOutbound(s, discard889)
		}},
		{"retire", func(_ *testing.T, _ *Processor, m *Manager, s *Stream) {
			s.flags.Or(flagAsyncRunning)
			m.retire(s)
		}},
		{"GOAWAY", func(t *testing.T, p *Processor, _ *Manager, _ *Stream) {
			if err := p.handleGoAway(&http2.GoAwayFrame{LastStreamID: 0}); err != nil {
				t.Fatal(err)
			}
		}},
		{"RemoveStreamFromMap", func(_ *testing.T, _ *Processor, m *Manager, s *Stream) { m.RemoveStreamFromMap(s.ID) }},
		{"Close", func(_ *testing.T, _ *Processor, m *Manager, _ *Stream) { m.Close() }},
		{"ExpireWrite", func(_ *testing.T, p *Processor, _ *Manager, s *Stream) { p.ExpireWrite(s.ID) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, m, _ := wdSetup889(t)
			s := stalled889(m, 1, 5000)
			stalled889(m, 3, 5000) // another stays only if the path is per stream
			if m.stalledStreams() != 2 {
				t.Fatalf("setup: %d records", m.stalledStreams())
			}
			tc.end(t, p, m, s)
			left := m.stalledStreams()
			switch tc.name {
			case "GOAWAY", "Close":
				if left != 0 {
					t.Fatalf("%d records left, want 0 (every stream above the last one is gone)", left)
				}
			default:
				if left != 1 {
					t.Fatalf("%d records left, want 1 (the other stream's)", left)
				}
			}
		})
	}
}

// ExpireWrite resets the stream once, with INTERNAL_ERROR, drops its bytes, takes
// it out of the connection and remembers the identifier; the second call, a stream
// the peer or a pool handler had already reset, writes nothing.
func TestExpireWriteResetsOnce889(t *testing.T) {
	p, m, rec := wdSetup889(t)
	s := stalled889(m, 1, 5000)
	p.ExpireWrite(1)
	if rec.count() != 1 || rec.rst[0] != (rstRecord889{1, http2.ErrCodeInternal}) {
		t.Fatalf("resets %+v, want one RST_STREAM(INTERNAL_ERROR) for stream 1", rec.rst)
	}
	if _, ok := m.GetStream(1); ok {
		t.Fatal("the stream is still in the connection")
	}
	if m.OutboundHeld() != 0 {
		t.Fatalf("%d bytes still charged to the outbound budget", m.OutboundHeld())
	}
	if !p.resetIDs.has(1) {
		t.Fatal("the identifier is not remembered as reset: the client's late frames would be errors")
	}
	_ = s
	p.ExpireWrite(1)
	if rec.count() != 1 {
		t.Fatalf("a second expiry wrote %d more resets", rec.count()-1)
	}
	// A stream reset by someone else first (a pool handler's own reset at #906's
	// bound cancels it): taken out, no second RST_STREAM.
	s3 := stalled889(m, 3, 5000)
	s3.Cancel()
	p.ExpireWrite(3)
	if rec.count() != 1 {
		t.Fatalf("a stream that was cancelled already got a second reset: %+v", rec.rst)
	}
	if _, ok := m.GetStream(3); ok {
		t.Fatal("the cancelled stream stayed in the connection")
	}
	if m.stalledStreams() != 0 {
		t.Fatalf("%d records left", m.stalledStreams())
	}
}

// A pool handler blocked in AwaitSendWindow under #906's absolute bound, on a
// stream D2 expires, wakes with the stream's cancellation, not with the deadline:
// it does not send a second RST_STREAM.
func TestExpireWriteWakesAPoolHandlerAsReset889(t *testing.T) {
	p, m, rec := wdSetup889(t)
	s := stalled889(m, 1, 5000)
	res := make(chan error, 1)
	go func() { res <- s.AwaitSendWindow(time.Now().Add(time.Minute)) }()
	for m.sendWindowWaiters.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	p.ExpireWrite(1)
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the waiter woke with %v, want context.Canceled (a reset), not the deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter was not woken by the expiry")
	}
	if rec.count() != 1 {
		t.Fatalf("%d resets", rec.count())
	}
}

// With a write deadline a conn whose only business is a stalled stream is
// exempt from the conn clocks (D2 resets the stream); without one, or with a
// pool handler waiting in AwaitSendWindow, it is not.
func TestServerOwesWithAWriteDeadline889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	s := stalled889(m, 1, 5000)
	s.SetState(StateHalfClosedRemote)
	if !m.ServerOwes() {
		t.Fatal("a stalled stream does not exempt the conn although D2 bounds it")
	}
	m.sendWindowWaiters.Add(1)
	if m.ServerOwes() {
		t.Fatal("a pool handler waiting for the window exempts the conn: its bound is #906's, and the conn clock stays")
	}
	m.sendWindowWaiters.Add(-1)
	m.SetWriteTimeout(0)
	if m.ServerOwes() {
		t.Fatal("without a write deadline a stalled stream exempts the conn (the old rule)")
	}
}

// The common path pays nothing: reserving window allocates nothing and touches no
// record while no stream is stalled.
func TestWriteDeadlineCommonPathIsFree889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	s := m.CreateStream(1)
	s.SetState(StateOpen)
	s.SetWindowSize(1 << 30)
	if a := testing.AllocsPerRun(100, func() { m.ReserveSendWindow(s, 1000) }); a != 0 {
		t.Fatalf("ReserveSendWindow allocated %.0f times", a)
	}
	if m.connReleased.Load() != 0 {
		t.Fatal("the conn's releases are counted with no stream stalled")
	}
	stalled889(m, 3, 100)
	m.ReserveSendWindow(s, 1000)
	if m.connReleased.Load() != 1000 {
		t.Fatalf("conn released %d, want 1000 while a stream is stalled", m.connReleased.Load())
	}
}

// BenchmarkOneByteGrantWriteDeadline889 is BenchmarkOneByteGrant911 with the write
// deadline on (off, it is that benchmark): the price of a stalled stream's flush,
// which takes the books' lock once to credit what it released.
func BenchmarkOneByteGrantWriteDeadline889(b *testing.B) {
	for _, on := range []bool{false, true} {
		name := "off"
		if on {
			name = "on"
		}
		b.Run(name, func(b *testing.B) {
			fw := newTestFrameWriter()
			p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), fw, newTestResponseWriter())
			m := p.GetManager()
			if on {
				m.SetWriteTimeout(time.Minute)
			}
			m.UpdateConnectionWindow(1 << 30)
			s := m.CreateStream(1)
			s.SetState(StateOpen)
			s.SetWindowSize(0)
			body := make([]byte, 1<<20)
			s.BufferOutbound(body, true)
			wu := makeWindowUpdateFrame(&testing.T{}, 1, 1)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if s.OutboundBuffer.Len() < 2 {
					b.StopTimer()
					s.BufferOutbound(body, true)
					b.StartTimer()
				}
				if err := p.ProcessFrame(context.Background(), wu); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// What a stalled stream's floor is made of, through the frames a client sends: a
// WINDOW_UPDATE that lets 70000 bytes out and a SETTINGS_INITIAL_WINDOW_SIZE raise
// that does are progress; a hundred WINDOW_UPDATEs of one byte are not.
func TestWriteProgressThroughFramesIsBytesReleased889(t *testing.T) {
	ctx := context.Background()
	mk := func(t *testing.T) (*Processor, *Manager) {
		p, m, _ := wdSetup889(t)
		stalled889(m, 1, 1<<20)
		return p, m
	}
	t.Run("window-update-of-70000", func(t *testing.T) {
		p, m := mk(t)
		if err := p.ProcessFrame(ctx, makeWindowUpdateFrame(t, 1, 70000)); err != nil {
			t.Fatal(err)
		}
		if out := m.ExpiredWrites(dueAt889(), make([]uint32, 0, 4)); len(out) != 0 {
			t.Fatalf("expired %v after a WINDOW_UPDATE that released 70000 bytes", out)
		}
	})
	t.Run("settings-initial-window-raise", func(t *testing.T) {
		p, m := mk(t)
		f := makeSettingsFrame(t, http2.Setting{ID: http2.SettingInitialWindowSize, Val: 65535 + 70000})
		if err := p.ProcessFrame(ctx, f); err != nil {
			t.Fatal(err)
		}
		if out := m.ExpiredWrites(dueAt889(), make([]uint32, 0, 4)); len(out) != 0 {
			t.Fatalf("expired %v after a SETTINGS raise that released 70000 bytes", out)
		}
	})
	t.Run("a-hundred-one-byte-updates", func(t *testing.T) {
		p, m := mk(t)
		for i := 0; i < 100; i++ {
			if err := p.ProcessFrame(ctx, makeWindowUpdateFrame(t, 1, 1)); err != nil {
				t.Fatal(err)
			}
		}
		if out := m.ExpiredWrites(dueAt889(), make([]uint32, 0, 4)); len(out) != 1 {
			t.Fatalf("expired %v, want the dribbled stream", out)
		}
	})
	t.Run("a-ping-and-a-conn-window-update-with-no-stream-in-need", func(t *testing.T) {
		p, m := mk(t)
		for i := 0; i < 50; i++ {
			if err := p.ProcessFrame(ctx, makeWindowUpdateFrame(t, 0, 1)); err != nil {
				t.Fatal(err)
			}
		}
		if out := m.ExpiredWrites(dueAt889(), make([]uint32, 0, 4)); len(out) != 1 {
			t.Fatalf("expired %v: a conn WINDOW_UPDATE does not move a stream whose own window is shut", out)
		}
	})
}

// A pool handler's direct send (nothing buffered) makes no record: only a buffer does.
func TestPoolDirectSendLeavesNoRecord889(t *testing.T) {
	_, m, _ := wdSetup889(t)
	s := m.CreateStream(1)
	s.SetState(StateOpen)
	s.flags.Or(flagAsyncRunning)
	s.SetWindowSize(1 << 20)
	n, _, onPool, err := m.StreamWrite(s, s.Gen(), make([]byte, 5000), discard889)
	if err != nil || !onPool || n != 5000 {
		t.Fatalf("StreamWrite = %d, onPool %v, %v", n, onPool, err)
	}
	if m.stalledStreams() != 0 || m.NextWriteDeadline() != 0 {
		t.Fatalf("a direct send left %d records, hint %d", m.stalledStreams(), m.NextWriteDeadline())
	}
}
