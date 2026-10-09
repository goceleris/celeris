package stream

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// celeris#904, review of celeris#965: a StreamWriter that outlives its handler
// holds the stream object, which goes back to a pool shared by every
// connection. The use token (Stream.Gen) and the lock the release takes keep
// its calls from reaching the object's next use.

type sent904 struct {
	n    int
	end  bool
	call int
}

func recorder904() (OutboundSend, *sent904) {
	r := &sent904{}
	return func(_ uint32, end bool, data []byte) { r.n += len(data); r.end = r.end || end; r.call++ }, r
}

// TestUseTokenGatesEverySend904: once the use is over (EndUse at the handler's
// return) every call that presents the old token is refused, and changes
// neither the stream's buffer, the budget, nor the windows.
func TestUseTokenGatesEverySend904(t *testing.T) {
	m := NewManager()
	s := m.CreateStream(1)
	s.SetState(StateOpen)
	send, rec := recorder904()
	gen := s.Gen()

	if err := m.SendOrBufferOutbound(s, gen, make([]byte, 70000), send); err != nil {
		t.Fatal(err)
	}
	if rec.n != 65535 || m.OutboundHeld() != 70000-65535 {
		t.Fatalf("before the use ended: %d bytes sent, %d held; want 65535 and %d", rec.n, m.OutboundHeld(), 70000-65535)
	}
	held, win, connWin, buffered := m.OutboundHeld(), s.GetWindowSize(), m.GetConnectionWindow(), s.OutboundBuffer.Len()

	s.EndUse()
	for _, c := range []struct {
		name string
		err  error
	}{
		{"SendOrBufferOutbound", m.SendOrBufferOutbound(s, gen, []byte("late"), send)},
		{"EndOutbound", m.EndOutbound(s, gen, send)},
		{"UseLock", func() error {
			err := s.UseLock(gen)
			if err == nil {
				s.UseUnlock()
			}
			return err
		}()},
		{"StreamWrite", func() error { _, _, _, err := m.StreamWrite(s, gen, []byte("late"), send); return err }()},
		{"AwaitSendWindowUse", func() error { var d time.Time; return s.AwaitSendWindowUse(gen, time.Second, &d) }()},
	} {
		if !errors.Is(c.err, ErrStreamEnded) {
			t.Errorf("%s with the ended use's token returned %v, want ErrStreamEnded", c.name, c.err)
		}
	}
	if rec.n != 65535 || rec.end || rec.call != 1 {
		t.Errorf("a refused call sent: %d bytes, end=%v, %d send calls; want only the first send", rec.n, rec.end, rec.call)
	}
	if m.OutboundHeld() != held || s.GetWindowSize() != win || m.GetConnectionWindow() != connWin || s.OutboundBuffer.Len() != buffered {
		t.Errorf("refused calls changed the state: held %d->%d, stream window %d->%d, connection window %d->%d, buffered %d->%d",
			held, m.OutboundHeld(), win, s.GetWindowSize(), connWin, m.GetConnectionWindow(), buffered, s.OutboundBuffer.Len())
	}
	// The new token of the same object is not refused: it is the next use's.
	if err := m.SendOrBufferOutbound(s, s.Gen(), []byte("x"), send); err != nil {
		t.Errorf("the current token was refused: %v", err)
	}
}

// TestCancelledStreamRefusesWrites904: after the peer's RST_STREAM (or the
// connection's close) Write returns the context error and Close queues
// nothing: RFC 9113 §5.1 allows no frame after a RST_STREAM, and bytes
// buffered for a dead stream would stay charged to the budget.
func TestCancelledStreamRefusesWrites904(t *testing.T) {
	m := NewManager()
	s := m.CreateStream(1)
	s.SetState(StateOpen)
	send, rec := recorder904()
	s.Cancel()
	if err := m.SendOrBufferOutbound(s, s.Gen(), []byte("late"), send); !errors.Is(err, context.Canceled) {
		t.Errorf("SendOrBufferOutbound on a cancelled stream returned %v, want context.Canceled", err)
	}
	if err := m.EndOutbound(s, s.Gen(), send); !errors.Is(err, context.Canceled) {
		t.Errorf("EndOutbound on a cancelled stream returned %v, want context.Canceled", err)
	}
	if rec.call != 0 || m.OutboundHeld() != 0 {
		t.Errorf("a cancelled stream's calls sent %d times and charged %d bytes", rec.call, m.OutboundHeld())
	}
}

// TestFlushOutboundLeavesAStreamOfAnotherManagerAlone904: a flush that finds a
// stream that is not its connection's (released, and taken by another
// connection) sends nothing and debits no window.
func TestFlushOutboundLeavesAStreamOfAnotherManagerAlone904(t *testing.T) {
	m1, m2 := NewManager(), NewManager()
	s := m1.CreateStream(1)
	s.BufferOutbound([]byte("abc"), true)
	send, rec := recorder904()
	if m2.FlushOutbound(s, send) || rec.call != 0 {
		t.Fatalf("a flush by another connection's manager sent %d times", rec.call)
	}
	if s.GetWindowSize() != 65535 || m2.GetConnectionWindow() != 65535 {
		t.Errorf("a flush by another connection's manager debited the windows: %d, %d", s.GetWindowSize(), m2.GetConnectionWindow())
	}
	if !m1.FlushOutbound(s, send) || rec.n != 3 || !rec.end {
		t.Errorf("the owner's flush: sent %d bytes, end=%v, want 3 and END_STREAM", rec.n, rec.end)
	}
}

// TestAbandonedBufferIsSentWithoutEndStreamAndFinishes904: an inline handler
// that returned without ending its response, with DATA buffered, has the buffer
// sent as the window opens, with no END_STREAM, and the flush reports the
// stream finished so it is released.
func TestAbandonedBufferIsSentWithoutEndStreamAndFinishes904(t *testing.T) {
	m := NewManager()
	s := m.CreateStream(1)
	s.SetState(StateOpen)
	s.SetWindowSize(10)
	send, rec := recorder904()
	if err := m.SendOrBufferOutbound(s, s.Gen(), make([]byte, 25), send); err != nil {
		t.Fatal(err)
	}
	if !s.AbandonOutbound() {
		t.Fatal("AbandonOutbound reported nothing pending")
	}
	s.SetWindowSize(10)
	if m.FlushOutbound(s, send) {
		t.Fatal("the stream was reported finished with 5 bytes still buffered")
	}
	s.SetWindowSize(100)
	if !m.FlushOutbound(s, send) {
		t.Fatal("the stream was not reported finished once its buffer was out")
	}
	if rec.n != 25 || rec.end {
		t.Errorf("sent %d bytes, END_STREAM=%v; want 25 and no END_STREAM", rec.n, rec.end)
	}
	if m.OutboundHeld() != 0 {
		t.Errorf("%d bytes still charged", m.OutboundHeld())
	}
	// A stream whose handler ended it, or one with nothing buffered, is not abandoned.
	s2 := m.CreateStream(3)
	if s2.AbandonOutbound() {
		t.Error("an empty stream was reported as having a buffer pending")
	}
}

// TestReleaseVersusLateWriters904: writers that hold a stream object race its
// release, as a goroutine the handler started does. Under -race this finds a
// release that resets the object without the lock the writers take. And
// whatever the interleaving, no byte may stay charged to the budget (a write
// that landed after the reset is never refunded) or buffered on the pooled
// object (the next use would send it).
func TestReleaseVersusLateWriters904(t *testing.T) {
	for i := 0; i < 200; i++ {
		m := NewManager()
		s := m.CreateStream(1)
		s.SetState(StateOpen)
		s.SetWindowSize(0) // everything written is buffered, so the budget is charged
		gen := s.Gen()
		send, _ := recorder904()
		var wg sync.WaitGroup
		var accepted atomic.Int64
		stop := make(chan struct{})
		for w := 0; w < 3; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					err := m.SendOrBufferOutbound(s, gen, []byte("0123456789"), send)
					if err == nil {
						accepted.Add(10)
						continue
					}
					if !errors.Is(err, ErrStreamEnded) && !errors.Is(err, context.Canceled) {
						t.Errorf("late write returned %v", err)
					}
					_ = m.EndOutbound(s, gen, send)
					return
				}
			}()
		}
		time.Sleep(time.Duration(i%5) * 20 * time.Microsecond)
		s.EndUse() // the handler returned
		s.Release()
		wg.Wait()
		close(stop)
		if held := m.OutboundHeld(); held != 0 {
			t.Fatalf("round %d: %d bytes stay charged to the connection after the stream was released (%d accepted)", i, held, accepted.Load())
		}
		// Nothing is left on the pooled object for its next use.
		if s2 := NewStream(7); s2.OutboundBuffer != nil && s2.OutboundBuffer.Len() > 0 || s2.OutboundEndStream {
			t.Fatalf("round %d: a stream taken from the pool starts with buffered output (%v bytes, end=%v)", i, s2.OutboundBuffer.Len(), s2.OutboundEndStream)
		} else {
			s2.Release()
		}
	}
}

// TestPoolWaiterIsRefusedWhenItsStreamEnds904: a StreamWriter that waits for
// window on a detached goroutine when the handler returns and the stream is
// released does not spin and does not touch the next use: its wait ends with
// an error.
func TestPoolWaiterIsRefusedWhenItsStreamEnds904(t *testing.T) {
	m := NewManager()
	s := m.CreateStream(1)
	s.SetState(StateOpen)
	s.SetWindowSize(0)
	s.flags.Or(flagAsyncRunning)
	gen := s.Gen()
	res := make(chan error, 1)
	go func() {
		var d time.Time
		res <- s.AwaitSendWindowUse(gen, time.Minute, &d)
	}()
	for until := time.Now().Add(5 * time.Second); m.sendWindowWaiters.Load() == 0; {
		if time.Now().After(until) {
			t.Fatal("the waiter never started to wait")
		}
		time.Sleep(time.Millisecond)
	}
	s.EndUse()
	s.Release()
	select {
	case err := <-res:
		if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("the waiter returned %v after its stream ended, want an error that is not the deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter was still waiting 5 s after its stream was released")
	}
}
