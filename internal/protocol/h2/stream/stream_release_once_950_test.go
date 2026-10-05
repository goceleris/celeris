package stream

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// celeris#950: a reset (the peer's RST_STREAM, or one the server sends) as a
// pool handler returns could release the handler's stream twice.
// DeleteStream took the stream out of the map under the manager's lock, and
// read flagAsyncRunning only after the unlock. The handler's goroutine could
// release the stream in between (its deferred cleanup: RemoveStreamFromMap,
// Release, and resetAndPool stores 0 in the flags), so DeleteStream saw no
// handler and released the stream again. The object was then in the stream
// pool twice, and two later streams, on any connections, shared it. The
// peer's reset also closed, marked and cancelled the stream after its
// lookup, outside the lock, when the object could already be another
// stream's (celeris#951).

// openPoolStream opens stream id on p as a HEADERS with END_STREAM for a
// pool handler would: in the map, half-closed (remote), with
// flagAsyncRunning set and the pool count raised, as runHandler leaves it
// before it submits the stream to the pool.
func openPoolStream(t *testing.T, p *Processor, id uint32) *Stream {
	t.Helper()
	s, ok := p.manager.TryOpenStream(id)
	if !ok {
		t.Fatalf("TryOpenStream(%d) refused", id)
	}
	s.SetState(StateHalfClosedRemote)
	s.flags.Or(flagAsyncRunning)
	p.poolRunning.Add(1)
	return s
}

// TestRSTAtPoolHandlerEndReleasesItsStreamOnce950 drives the interleaving
// that released a stream twice, deterministically, for a RST_STREAM from the
// peer (handleRSTStream) and for one the server sends (a stream error:
// sendRSTStreamAndMarkClosed, which deletes the stream with DeleteStream).
// The test holds the manager's windowUpdateMu, which both take after they
// have taken the stream out of the map, so the reset stops there. The pool
// handler's goroutine then ends the stream as executeHandler does, and
// another connection's stream takes the object from the pool. When the
// reset goes on, it must leave that stream alone: the pool handler released
// the object, and it is no longer the reset stream.
func TestRSTAtPoolHandlerEndReleasesItsStreamOnce950(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reset func(p *Processor) error
	}{
		{"peer-rst", func(p *Processor) error {
			return p.handleRSTStream(&http2.RSTStreamFrame{
				FrameHeader: http2.FrameHeader{Type: http2.FrameRSTStream, StreamID: 1},
				ErrCode:     http2.ErrCodeCancel,
			})
		}},
		{"server-rst", func(p *Processor) error {
			return p.sendRSTStreamAndMarkClosed(1, http2.ErrCodeFlowControl)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fromPool, ran := 0, 0
			defer func() {
				t.Logf("the other connection's stream took the object from the pool in %d of %d iterations (the others hand it over directly)", fromPool, ran)
			}()
			for i := range 20 {
				ran++
				p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), newTestFrameWriter(), newTestResponseWriter())
				m := p.manager
				s := openPoolStream(t, p, 1)

				m.windowUpdateMu.Lock()
				rstErr := make(chan error, 1)
				go func() { rstErr <- tc.reset(p) }()
				for deadline := time.Now().Add(10 * time.Second); ; runtime.Gosched() {
					if _, in := m.GetStream(1); !in {
						break
					}
					if time.Now().After(deadline) {
						m.windowUpdateMu.Unlock()
						t.Fatal("the reset never took stream 1 out of the map")
					}
				}

				// The handler returns, and its goroutine ends the stream as
				// executeHandler's deferred cleanup does (the reset already
				// took it out of the map, so RemoveStreamFromMap has nothing
				// to do).
				s.SetState(StateClosed)
				s.Release()

				// Another connection's stream gets the object from the pool.
				// The race detector drops a quarter of sync.Pool's Puts; then
				// the object is in no pool, and that stream gets it as
				// NewStream would hand it over.
				m2 := NewManager()
				next, nextID := (*Stream)(nil), uint32(0)
				for id := uint32(7); id < 7+2*64; id += 2 {
					x, ok := m2.TryOpenStream(id)
					if !ok {
						break
					}
					if x == s {
						next, nextID = x, id
						fromPool++
						break
					}
				}
				if next == nil {
					nextID = 1001
					s.ID = nextID
					s.manager = m2
					s.state.Store(int32(StateOpen))
					m2.mu.Lock()
					m2.streams[nextID] = s
					m2.mu.Unlock()
					m2.activeStreams.Add(1)
					next = s
				}
				nextCtx := next.Context()
				active := m2.CountActiveStreams()

				m.windowUpdateMu.Unlock()
				if err := <-rstErr; err != nil {
					t.Fatalf("reset: %v", err)
				}

				if next.ID != nextID || next.GetState() != StateOpen || next.IsCancelled() || nextCtx.Err() != nil {
					t.Fatalf("iteration %d: the reset of stream 1 released its object again after stream 1's pool handler had, "+
						"and the object was another connection's stream %d by then: it now reads ID %d, state %v, cancelled %v, context Err %v",
						i, nextID, next.ID, next.GetState(), next.IsCancelled(), nextCtx.Err())
				}
				if got, in := m2.GetStream(nextID); !in || got != next || m2.CountActiveStreams() != active {
					t.Fatalf("iteration %d: the other connection's stream %d lost its place: in map %v, active %d, want %d",
						i, nextID, in, m2.CountActiveStreams(), active)
				}
				m2.Close()
			}
		})
	}
}

// TestRSTRacesPoolHandlerEnd950 runs the real executeHandler against a reset
// of its stream (the peer's RST_STREAM, and a RST the server sends) many
// times, and then drains the stream pool: a stream released twice comes out
// of it twice. The window is narrow, and this rarely catches a double
// release by itself; under -race the detector reports every access of the
// stream that is not ordered against its release (the reset's touches of
// the stream, and a second release), which fails the test.
func TestRSTRacesPoolHandlerEnd950(t *testing.T) {
	n := 20000
	if testing.Short() {
		n = 2000
	}
	for _, tc := range []struct {
		name  string
		reset func(p *Processor)
	}{
		{"peer-rst", func(p *Processor) {
			_ = p.handleRSTStream(&http2.RSTStreamFrame{
				FrameHeader: http2.FrameHeader{Type: http2.FrameRSTStream, StreamID: 1},
				ErrCode:     http2.ErrCodeCancel,
			})
		}},
		{"server-rst", func(p *Processor) { _ = p.sendRSTStreamAndMarkClosed(1, http2.ErrCodeFlowControl) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			double := 0
			for i := range n {
				p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), newTestFrameWriter(), newTestResponseWriter())
				s := openPoolStream(t, p, 1)
				start := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); <-start; p.executeHandler(s) }()
				go func() { defer wg.Done(); <-start; tc.reset(p) }()
				close(start)
				wg.Wait()
				seen := 0
				for range 64 {
					if streamPool.Get().(*Stream) == s {
						seen++
					}
				}
				if seen > 1 {
					double++
					t.Errorf("iteration %d: the stream came out of the stream pool %d times: it was released twice", i, seen)
				}
				if i%1000 == 999 {
					runtime.GC()
				}
			}
			if double > 0 {
				t.Errorf("%d of %d resets released their stream twice", double, n)
			}
		})
	}
}

// celeris#948: a pool stream reset while its response was still buffered for
// the peer's window was never released. The reset took it out of the map
// and left its release to its handler's goroutine (flagAsyncRunning); when
// the handler returned, its cleanup found the response buffered and handed
// the stream to the event loop's WINDOW_UPDATE flush, which looks streams up
// in the map and so never found it. Its buffered bytes stayed charged to the
// connection's outbound budget (celeris#893) for good, and a client that
// repeats this pushes its connection over the budget. Both orders are
// checked: the reset before the handler's cleanup, and after it.
func TestPoolStreamResetWhileBufferedIsReleased948(t *testing.T) {
	const body = 1000
	for _, rstFirst := range []bool{true, false} {
		name := "rst-after-cleanup"
		if rstFirst {
			name = "rst-before-cleanup"
		}
		t.Run(name, func(t *testing.T) {
			buffered := make(chan context.Context, 1)
			proceed := make(chan struct{})
			p, m := newBudgetProcessor893(t, HandlerFunc(func(_ context.Context, s *Stream) error {
				// The window refused the response: it is buffered, as
				// the conn layer's writer does with what does not fit.
				if !s.TryBufferOutbound(make([]byte, body), true) {
					return errors.New("TryBufferOutbound refused an empty budget")
				}
				buffered <- s.Context()
				<-proceed
				return nil
			}))
			s := openPoolStream(t, p, 1)
			s.SetWindowSize(0)
			ran := make(chan struct{})
			go func() { defer close(ran); p.executeHandler(s) }()
			ctx := <-buffered
			if got := m.OutboundHeld(); got != body {
				t.Fatalf("the handler buffered %d bytes and the budget holds %d", body, got)
			}
			rst := func() {
				t.Helper()
				if err := p.ProcessFrame(context.Background(), makeRSTStreamFrame(t, 1, http2.ErrCodeCancel)); err != nil {
					t.Fatalf("RST_STREAM: %v", err)
				}
			}
			if rstFirst {
				rst()
				close(proceed)
				<-ran
			} else {
				close(proceed)
				<-ran
				if _, in := m.GetStream(1); !in {
					t.Fatal("the handler returned with its response buffered, and its stream is not in the map for the WINDOW_UPDATE flush")
				}
				rst()
			}
			if got := m.OutboundHeld(); got != 0 {
				t.Fatalf("the stream was reset and its handler has returned, and its %d buffered bytes are still charged to the "+
					"connection's outbound budget: the stream was never released", got)
			}
			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatalf("the reset stream's context: Err = %v, want context.Canceled", ctx.Err())
			}
			if n := m.StreamCount(); n != 0 {
				t.Fatalf("%d streams left in the map", n)
			}
		})
	}
}
