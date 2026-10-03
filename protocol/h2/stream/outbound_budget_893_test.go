package stream

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// celeris#893: the connection's outbound budget. These pin the accounting and
// the two rules built on it (no inline handler over the budget; a pool handler
// waits instead of buffering past it). The end-to-end check, heap and wire, is
// TestH2SilentClientCannotPinResponseBodies893 in the root package.

func newBudgetProcessor893(t *testing.T, h Handler) (*Processor, *Manager) {
	t.Helper()
	if h == nil {
		h = HandlerFunc(func(context.Context, *Stream) error { return nil })
	}
	p := NewProcessor(h, newTestFrameWriter(), newTestResponseWriter())
	return p, p.GetManager()
}

// openStream893 opens a stream as an inbound HEADERS would, without running a
// handler: in the manager's map, with the default send window.
func openStream893(t *testing.T, m *Manager, id uint32) *Stream {
	t.Helper()
	s := m.CreateStream(id)
	s.SetState(StateOpen)
	return s
}

func TestOutboundBudgetAccounting893(t *testing.T) {
	p, m := newBudgetProcessor893(t, nil)
	s := openStream893(t, m, 1)
	s.SetWindowSize(0)
	s.BufferOutbound(make([]byte, 1000), true)
	if got := m.OutboundHeld(); got != 1000 {
		t.Fatalf("after buffering 1000 bytes: held %d, want 1000", got)
	}

	// A WINDOW_UPDATE sends 400 of them: 600 still held.
	if err := p.ProcessFrame(context.Background(), makeWindowUpdateFrame(t, 1, 400)); err != nil {
		t.Fatal(err)
	}
	if got := m.OutboundHeld(); got != 600 {
		t.Fatalf("after a 400-byte WINDOW_UPDATE: held %d, want 600", got)
	}

	// SETTINGS_INITIAL_WINDOW_SIZE raised by 250: the stream's window goes
	// to 250 and that much more is sent.
	if err := p.ProcessFrame(context.Background(), makeSettingsFrame(t, http2.Setting{ID: http2.SettingInitialWindowSize, Val: 65535 + 250})); err != nil {
		t.Fatal(err)
	}
	if got := m.OutboundHeld(); got != 350 {
		t.Fatalf("after SETTINGS_INITIAL_WINDOW_SIZE +250: held %d, want 350", got)
	}

	// The peer resets the stream: what it held is dropped with it.
	if err := p.ProcessFrame(context.Background(), makeRSTStreamFrame(t, 1, http2.ErrCodeCancel)); err != nil {
		t.Fatal(err)
	}
	if got := m.OutboundHeld(); got != 0 {
		t.Fatalf("after RST_STREAM: held %d, want 0", got)
	}

	// A released stream's inline reuse refunds too.
	s2 := openStream893(t, m, 3)
	s2.BufferOutbound(make([]byte, 77), true)
	ResetH2StreamInline(s2, 5)
	if got := m.OutboundHeld(); got != 0 {
		t.Fatalf("after ResetH2StreamInline: held %d, want 0", got)
	}
}

func TestTryBufferOutboundRespectsTheBudget893(t *testing.T) {
	_, m := newBudgetProcessor893(t, nil)
	a := openStream893(t, m, 1)
	a.flags.Or(flagAsyncRunning)
	// Nothing held: any one body is buffered whole, even past the budget.
	if !a.TryBufferOutbound(make([]byte, OutboundBudget+1), true) {
		t.Fatal("with nothing held, a body over the budget was refused")
	}
	b := openStream893(t, m, 3)
	b.flags.Or(flagAsyncRunning)
	if b.TryBufferOutbound(make([]byte, 1), true) {
		t.Fatal("over the budget, a pool handler's 1 byte was buffered")
	}
	if b.OutboundBuffer != nil && b.OutboundBuffer.Len() != 0 {
		t.Fatal("a refused TryBufferOutbound buffered something")
	}
	if got := m.OutboundHeld(); got != OutboundBudget+1 {
		t.Fatalf("held %d, want %d (a refusal must not charge)", got, OutboundBudget+1)
	}
	// An inline (event-loop) stream cannot wait: it is always buffered.
	c := openStream893(t, m, 5)
	if !c.TryBufferOutbound(make([]byte, 10), true) || c.OutboundBuffer.Len() != 10 {
		t.Fatal("an inline stream's data was not buffered")
	}
	// Room again once the first is gone. Its pool handler has returned
	// (executeHandler clears the flag when it leaves data buffered), so the
	// delete releases it.
	a.flags.And(^flagAsyncRunning)
	m.DeleteStream(1)
	m.DeleteStream(5)
	if got := m.OutboundHeld(); got != 0 {
		t.Fatalf("after deleting both holders: held %d, want 0", got)
	}
	if !b.TryBufferOutbound(make([]byte, 1), true) {
		t.Fatal("with nothing held, the pool handler's byte was refused")
	}
	// Up to the budget, not past it.
	b2 := openStream893(t, m, 7)
	b2.flags.Or(flagAsyncRunning)
	if !b2.TryBufferOutbound(make([]byte, OutboundBudget-1), true) {
		t.Fatal("a body that fills the budget exactly was refused")
	}
	if b2.TryBufferOutbound(make([]byte, 1), true) {
		t.Fatal("a byte past a full budget was buffered")
	}
}

func TestAwaitSendWindowWakes893(t *testing.T) {
	cases := []struct {
		name   string
		wake   func(p *Processor, m *Manager, waiter *Stream)
		wantOK bool
	}{
		{"stream-window", func(p *Processor, _ *Manager, waiter *Stream) {
			_ = p.ProcessFrame(context.Background(), makeWindowUpdateFrame(t, waiter.ID, 100))
		}, true},
		{"raw-stream-window", func(p *Processor, _ *Manager, waiter *Stream) {
			_ = p.HandleRawWindowUpdate(waiter.ID, []byte{0, 0, 0, 100})
		}, true},
		{"settings-initial-window", func(p *Processor, _ *Manager, _ *Stream) {
			_ = p.ProcessFrame(context.Background(), makeSettingsFrame(t, http2.Setting{ID: http2.SettingInitialWindowSize, Val: 65535 + 100}))
		}, true},
		{"waiter-reset", func(p *Processor, _ *Manager, waiter *Stream) {
			_ = p.ProcessFrame(context.Background(), makeRSTStreamFrame(t, waiter.ID, http2.ErrCodeCancel))
		}, false},
		{"conn-closed", func(_ *Processor, m *Manager, _ *Stream) { m.Close() }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, m := newBudgetProcessor893(t, nil)
			waiter := openStream893(t, m, 3)
			waiter.flags.Or(flagAsyncRunning)
			waiter.SetWindowSize(0)

			var returned atomic.Int32 // 0 running, 1 true, 2 false
			go func() {
				if waiter.AwaitSendWindow() {
					returned.Store(1)
				} else {
					returned.Store(2)
				}
			}()
			time.Sleep(50 * time.Millisecond)
			if r := returned.Load(); r != 0 {
				t.Fatalf("AwaitSendWindow returned (%d) with the stream window closed", r)
			}
			if n := m.sendWindowWaiters.Load(); n != 1 {
				t.Fatalf("waiters %d, want 1 (the waiter must be counted before it sleeps)", n)
			}
			// A refund of the budget is not a window: it must not wake the
			// waiter into a busy loop.
			holder := openStream893(t, m, 1)
			holder.BufferOutbound(make([]byte, 10), true)
			m.DeleteStream(1)
			time.Sleep(20 * time.Millisecond)
			if r := returned.Load(); r != 0 {
				t.Fatalf("AwaitSendWindow returned (%d) on a budget refund, with its window still closed", r)
			}
			tc.wake(p, m, waiter)
			for until := time.Now().Add(2 * time.Second); returned.Load() == 0 && time.Now().Before(until); {
				time.Sleep(time.Millisecond)
			}
			want := int32(1)
			if !tc.wantOK {
				want = 2
			}
			if r := returned.Load(); r != want {
				t.Fatalf("%s: AwaitSendWindow result %d, want %d (1 = window open, 2 = stream gone, 0 = still waiting)", tc.name, r, want)
			}
		})
	}
}

// TestNoInlineHandlerOverTheBudget893: a connection over its budget runs a new
// stream's handler on the worker pool, not inline; under it, inline again.
func TestNoInlineHandlerOverTheBudget893(t *testing.T) {
	ran := make(chan bool, 4)
	var p *Processor
	h := HandlerFunc(func(_ context.Context, s *Stream) error {
		ran <- s.ResponseWriter == p.InlineWriter
		return nil
	})
	p, m := newBudgetProcessor893(t, h)
	p.InlineWriter = newTestResponseWriter()
	hdrs := encodeHeaders(t, [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", "x"}})
	run := func(id uint32) bool {
		t.Helper()
		if err := p.ProcessFrame(context.Background(), makeHeadersFrame(t, id, true, true, hdrs)); err != nil {
			t.Fatal(err)
		}
		select {
		case inline := <-ran:
			return inline
		case <-time.After(2 * time.Second):
			t.Fatalf("stream %d: the handler did not run", id)
			return false
		}
	}
	if !run(1) {
		t.Fatal("under the budget, a GET did not run inline")
	}
	holder := openStream893(t, m, 3)
	holder.SetWindowSize(0)
	holder.BufferOutbound(make([]byte, OutboundBudget), true)
	inlineBefore := p.InlineCount
	if run(5) {
		t.Fatal("over the budget, a GET ran inline")
	}
	if p.InlineCount != inlineBefore {
		t.Fatalf("InlineCount %d -> %d over the budget", inlineBefore, p.InlineCount)
	}
	m.DeleteStream(3)
	if !run(7) {
		t.Fatal("back under the budget, a GET did not run inline")
	}
}

// TestBudgetPoolStreamIsNeverBuffered893: a GET that would have run inline
// but runs on the pool because its connection is over the budget is never
// given a copy, even once the budget has room again: its response goes
// through the write queue alone, behind its HEADERS (TryBufferOutbound). A
// stream that is on the pool for another reason (an async route, a request
// body) is still copied when the budget has room.
func TestBudgetPoolStreamIsNeverBuffered893(t *testing.T) {
	freed := make(chan struct{})
	got := make(chan [2]bool, 1)
	var p *Processor
	h := HandlerFunc(func(_ context.Context, s *Stream) error {
		if s.ResponseWriter == p.InlineWriter {
			got <- [2]bool{false, false} // inline, the event loop would wait here: do not
			return nil
		}
		<-freed
		got <- [2]bool{true, s.TryBufferOutbound(make([]byte, 10), true)}
		return nil
	})
	p, m := newBudgetProcessor893(t, h)
	p.InlineWriter = newTestResponseWriter()
	holder := openStream893(t, m, 3)
	holder.SetWindowSize(0)
	holder.BufferOutbound(make([]byte, OutboundBudget), true)
	hdrs := encodeHeaders(t, [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", "x"}})
	if err := p.ProcessFrame(context.Background(), makeHeadersFrame(t, 5, true, true, hdrs)); err != nil {
		t.Fatal(err)
	}
	m.DeleteStream(3)
	if held := m.OutboundHeld(); held != 0 {
		t.Fatalf("held %d after the holder was deleted, want 0", held)
	}
	close(freed)
	select {
	case r := <-got:
		if !r[0] {
			t.Fatal("over the budget, the GET ran inline")
		}
		if r[1] {
			t.Fatal("a GET on the pool for the budget was buffered once the budget had room: its DATA could overtake its queued HEADERS")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the handler did not run")
	}
	if held := m.OutboundHeld(); held != 0 {
		t.Fatalf("held %d, want 0 (a refusal must not charge)", held)
	}
	// On the pool for another reason: copied when the budget has room.
	a := openStream893(t, m, 7)
	a.flags.Or(flagAsyncRunning)
	if !a.TryBufferOutbound(make([]byte, 10), true) || a.OutboundBuffer.Len() != 10 {
		t.Fatal("an async stream's data was not buffered with the budget free")
	}
}
