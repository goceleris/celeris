package stream

import (
	"context"
	"encoding/binary"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// celeris#951: the event loop looks a stream up in its connection's map and
// then keeps using the *Stream, while the stream's pool handler can return on
// its own goroutine and release it to the stream pool. The object is then any
// connection's next stream, and the loop sets its state (and, through its
// manager field, adjusts that connection's MAX_CONCURRENT_STREAMS count or
// dereferences nil), cancels its context, credits its window.
//
// The rule that fixes it: a stream a pool handler runs on goes to the stream
// pool only from the event loop, at the end of a frame batch
// (FlushInlineCleanup) or at Close (Manager.retire, drainRetired). These
// tests drive the real executeHandler against each event the loop handles.

// reusedBy951 takes the object s from the stream pool the way another
// connection's HEADERS would (TryOpenStream), and returns the stream m2 has
// opened on it, or nil when s is not in the pool. The race detector drops a
// quarter of sync.Pool's Puts, so callers repeat the experiment.
func reusedBy951(m2 *Manager, s *Stream) (*Stream, uint32) {
	for id := uint32(7); id < 7+2*64; id += 2 {
		x, ok := m2.TryOpenStream(id)
		if !ok {
			break
		}
		if x == s {
			return x, id
		}
	}
	return nil, 0
}

// TestPoolStreamIsNotPooledWhileTheLoopHoldsIt951 stops the event loop in the
// middle of a WINDOW_UPDATE for stream 1, after it has looked the stream up
// and credited its window, and lets the stream's pool handler end then (the
// real executeHandler). The loop still holds the *Stream, so the object must
// not be in the stream pool, where another connection's stream can get it:
// before the fix the handler's goroutine released it at once and the other
// connection's stream got it, with the loop about to act on it (set its
// state, cancel its context, credit its window).
func TestPoolStreamIsNotPooledWhileTheLoopHoldsIt951(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event func(p *Processor) error
	}{
		{"window-update", func(p *Processor) error {
			return p.handleWindowUpdate(&http2.WindowUpdateFrame{
				FrameHeader: http2.FrameHeader{Type: http2.FrameWindowUpdate, StreamID: 1}, Increment: 1000})
		}},
		{"raw-window-update", func(p *Processor) error {
			var payload [4]byte
			binary.BigEndian.PutUint32(payload[:], 1000)
			return p.HandleRawWindowUpdate(1, payload[:])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pooled, ran := 0, 0
			for i := range 24 {
				ran++
				p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), newTestFrameWriter(), newTestResponseWriter())
				m := p.manager
				s := openPoolStream(t, p, 1)
				orig := s.GetWindowSize()

				// The WINDOW_UPDATE stops at notifySendWindow, the first
				// lock it takes after the lookup and the window credit.
				m.sendWindowWaiters.Store(1)
				m.sendWindowMu.Lock()
				evErr := make(chan error, 1)
				go func() { evErr <- tc.event(p) }()
				for deadline := time.Now().Add(10 * time.Second); s.GetWindowSize() == orig; runtime.Gosched() {
					if time.Now().After(deadline) {
						m.sendWindowMu.Unlock()
						t.Fatal("the WINDOW_UPDATE never credited stream 1's window")
					}
				}

				// The handler returns while the loop holds the stream.
				p.executeHandler(s)

				m2 := NewManager()
				next, nextID := reusedBy951(m2, s)
				if next != nil {
					pooled++
				}

				m.sendWindowMu.Unlock()
				m.sendWindowWaiters.Store(0)
				if err := <-evErr; err != nil {
					t.Fatalf("iteration %d: %v", i, err)
				}
				if next != nil {
					t.Errorf("iteration %d: stream 1's pool handler released its object while the event loop held it, and another "+
						"connection's stream %d got it (state %v, cancelled %v): the loop acts on it next", i, nextID, next.GetState(), next.IsCancelled())
				}

				// The loop's batch ends: now the object is released, once.
				p.FlushInlineCleanup()
				if s.manager != nil && next == nil {
					t.Errorf("iteration %d: the batch ended and stream 1 was not released", i)
				}
				m2.Close()
			}
			t.Logf("the object was in the stream pool under the loop in %d of %d iterations", pooled, ran)
		})
	}
}

// newBystander951 starts the other connection: it opens streams on a manager
// of its own, as HEADERS on another connection would (they come out of the
// stream pool), and checks that nothing it did not do happens to them while it
// holds them. stop ends it; bad counts what it found, and first holds the
// first report.
func newBystander951(stop <-chan struct{}, bad *atomic.Int64, first *atomic.Value) (done <-chan struct{}) {
	d := make(chan struct{})
	go func() {
		defer close(d)
		m2 := NewManager()
		id := uint32(1001)
		for {
			select {
			case <-stop:
				m2.Close()
				return
			default:
			}
			id += 2
			x, ok := m2.TryOpenStream(id)
			if !ok {
				m2.Close()
				m2 = NewManager()
				continue
			}
			ctx := x.Context()
			for range 3 {
				runtime.Gosched()
			}
			if x.ID != id || x.GetState() != StateOpen || x.IsCancelled() || ctx.Err() != nil ||
				x.GetWindowSize() != 65535 || m2.CountActiveStreams() != 1 {
				if bad.Add(1) == 1 {
					first.Store(struct {
						id, got uint32
						st      State
						cancel  bool
						win     int32
						active  int
					}{id, x.ID, x.GetState(), x.IsCancelled(), x.GetWindowSize(), m2.CountActiveStreams()})
				}
			}
			m2.DeleteStream(id)
		}
	}()
	return d
}

// TestEventLoopRacesPoolHandlerEnd951 runs the real executeHandler on its own
// goroutine against each event the loop handles for the same stream (with
// and without a response buffered for the peer's window), the loop ending its
// frame batch after the event, while another connection keeps opening streams
// from the same pool. Under -race it reports every access of the stream that
// is not ordered against its release; without it the other connection's
// checks catch the loop acting on a stream that is not its own.
func TestEventLoopRacesPoolHandlerEnd951(t *testing.T) {
	n := 20000
	if testing.Short() {
		n = 2000
	}
	rst := func(p *Processor) {
		_ = p.handleRSTStream(&http2.RSTStreamFrame{FrameHeader: http2.FrameHeader{Type: http2.FrameRSTStream, StreamID: 1}, ErrCode: http2.ErrCodeCancel})
	}
	srst := func(p *Processor) { _ = p.sendRSTStreamAndMarkClosed(1, http2.ErrCodeFlowControl) }
	wu := func(p *Processor) {
		_ = p.handleWindowUpdate(&http2.WindowUpdateFrame{FrameHeader: http2.FrameHeader{Type: http2.FrameWindowUpdate, StreamID: 1}, Increment: 1 << 20})
	}
	rawWU := func(p *Processor) {
		var payload [4]byte
		binary.BigEndian.PutUint32(payload[:], 1<<20)
		_ = p.HandleRawWindowUpdate(1, payload[:])
	}
	connWU := func(p *Processor) {
		_ = p.handleWindowUpdate(&http2.WindowUpdateFrame{FrameHeader: http2.FrameHeader{Type: http2.FrameWindowUpdate, StreamID: 0}, Increment: 1 << 20})
	}
	settings := func(p *Processor) {
		_ = p.handleSettings(makeSettingsFrame(t, http2.Setting{ID: http2.SettingInitialWindowSize, Val: 1 << 20}).(*http2.SettingsFrame))
	}
	goaway := func(p *Processor) { _ = p.handleGoAway(&http2.GoAwayFrame{LastStreamID: 0}) }
	data := func(p *Processor) {
		_ = p.handleData(context.Background(), makeDataFrame(t, 1, false, []byte("late")).(*http2.DataFrame))
	}
	seq := func(fs ...func(*Processor)) func(*Processor) {
		return func(p *Processor) {
			for _, f := range fs {
				f(p)
			}
		}
	}
	for _, tc := range []struct {
		name     string
		buffered bool
		ev       func(*Processor)
	}{
		{"rst", false, rst}, {"server-rst", false, srst}, {"window-update", false, wu}, {"raw-window-update", false, rawWU},
		{"conn-window-update", false, connWU}, {"settings", false, settings}, {"goaway", false, goaway}, {"data-after-request", false, data},
		{"rst-buffered", true, rst}, {"server-rst-buffered", true, srst}, {"window-update-buffered", true, wu},
		{"raw-window-update-buffered", true, rawWU}, {"conn-window-update-buffered", true, connWU},
		{"settings-buffered", true, settings}, {"goaway-buffered", true, goaway},
		{"window-update-then-rst-buffered", true, seq(wu, rst)}, {"window-update-then-server-rst-buffered", true, seq(wu, srst)},
		// A detached stream is reset when its use ends but never pooled.
		{"detached-window-update", false, wu}, {"detached-server-rst", false, srst}, {"detached-data-after-request", false, data},
		{"detached-window-update-buffered", true, wu}, {"detached-settings-buffered", true, settings},
	} {
		detached := len(tc.name) > 9 && tc.name[:9] == "detached-"
		t.Run(tc.name, func(t *testing.T) {
			var bad atomic.Int64
			var first atomic.Value
			stop := make(chan struct{})
			bystander := newBystander951(stop, &bad, &first)
			defer func() {
				close(stop)
				<-bystander
				if b := bad.Load(); b > 0 {
					t.Errorf("another connection's streams were changed by this connection's event loop %d times; first: %+v", b, first.Load())
				}
			}()
			for i := range n {
				buffered := tc.buffered
				p := NewProcessor(HandlerFunc(func(_ context.Context, s *Stream) error {
					if detached {
						s.MarkDetached()
					}
					if buffered {
						s.SetHeadersSent()
						s.BufferOutbound(make([]byte, 1000), true)
					}
					return nil
				}), newTestFrameWriter(), newTestResponseWriter())
				s := openPoolStream(t, p, 1)
				s.AddHeadersBatch([][2]string{{":method", "GET"}, {":path", "/"}, {"x-a", "b"}})
				if buffered {
					s.SetWindowSize(0)
				}
				start := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); <-start; p.executeHandler(s) }()
				go func() { // the event loop: an event, then the end of its frame batch
					defer wg.Done()
					<-start
					tc.ev(p)
					p.FlushInlineCleanup()
				}()
				close(start)
				wg.Wait()
				p.manager.Close() // the connection closes
				if i%1000 == 999 {
					runtime.GC()
				}
			}
		})
	}
}

// TestRetiredStreamIsReleasedByTheLoopOnce951 follows one pool stream through
// its end: the handler returns (the stream is out of the map, its context is
// over, its budget is back), the object stays out of the stream pool until
// the loop's batch ends, and then it is released exactly once.
func TestRetiredStreamIsReleasedByTheLoopOnce951(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		name := "nothing-buffered"
		if buffered {
			name = "buffered-and-reset"
		}
		t.Run(name, func(t *testing.T) {
			for i := range 24 {
				bufferedCh, proceed := make(chan struct{}), make(chan struct{})
				p := NewProcessor(HandlerFunc(func(_ context.Context, s *Stream) error {
					if buffered {
						s.SetHeadersSent()
						s.BufferOutbound(make([]byte, 1000), true)
						close(bufferedCh)
						<-proceed
					}
					return nil
				}), newTestFrameWriter(), newTestResponseWriter())
				m := p.manager
				s := openPoolStream(t, p, 1)
				if buffered {
					s.SetWindowSize(0)
				}
				ctx := s.Context()
				ran := make(chan struct{})
				go func() { defer close(ran); p.executeHandler(s) }()
				if buffered {
					// The peer's RST takes the stream out of the map and
					// leaves its release to the handler's goroutine, which
					// returns with its response still buffered.
					<-bufferedCh
					if err := p.handleRSTStream(&http2.RSTStreamFrame{FrameHeader: http2.FrameHeader{Type: http2.FrameRSTStream, StreamID: 1}, ErrCode: http2.ErrCodeCancel}); err != nil {
						t.Fatal(err)
					}
					close(proceed)
				}
				<-ran
				if m.StreamCount() != 0 || m.OutboundHeld() != 0 || m.CountActiveStreams() != 0 {
					t.Fatalf("iteration %d: the handler returned and %d streams remain in the map, %d bytes are held, %d active",
						i, m.StreamCount(), m.OutboundHeld(), m.CountActiveStreams())
				}
				if ctx.Err() == nil {
					t.Fatalf("iteration %d: the stream's context is not over after its handler returned", i)
				}
				// Not released yet: the loop has not ended its batch.
				if s.manager == nil {
					t.Fatalf("iteration %d: the stream was reset before the event loop released it", i)
				}
				if buffered {
					// Its response bytes are dropped with the request: the
					// buffer is back in the pool, not held with its capacity
					// until the loop gets to the stream.
					s.mu.RLock()
					buf := s.OutboundBuffer
					s.mu.RUnlock()
					if buf != nil {
						t.Fatalf("iteration %d: the retired stream still holds its outbound buffer (cap %d)", i, buf.Cap())
					}
				}
				for range 64 {
					if got := streamPool.Get().(*Stream); got == s {
						t.Fatalf("iteration %d: the retired stream was in the stream pool before the loop's batch ended", i)
					}
				}
				p.FlushInlineCleanup()
				if s.manager != nil {
					t.Fatalf("iteration %d: the batch ended and the stream was not released", i)
				}
				// The queue is empty and keeps no pointer to a stream that is
				// pooled now: a stale entry would release another
				// connection's stream at the next batch.
				m.mu.Lock()
				left, flagged := len(m.retired), m.hasRetired.Load()
				var held int
				for _, q := range m.retired[:cap(m.retired)] {
					if q != nil {
						held++
					}
				}
				for _, q := range m.drainBuf[:cap(m.drainBuf)] {
					if q != nil {
						held++
					}
				}
				m.mu.Unlock()
				if left != 0 || flagged || held != 0 {
					t.Fatalf("iteration %d: after the drain %d streams are queued (flag %v) and %d pointers to released streams are kept", i, left, flagged, held)
				}
				p.FlushInlineCleanup() // nothing is retired now
				seen := 0
				for range 64 {
					if streamPool.Get().(*Stream) == s {
						seen++
					}
				}
				if seen > 1 {
					t.Fatalf("iteration %d: the stream came out of the stream pool %d times: it was released twice", i, seen)
				}
			}
		})
	}
}

// TestRetiredStreamsGoToThePoolAtClose951 covers the two ends of a connection
// that closes: streams retired before Close are released by it, and a handler
// that returns after Close releases its stream itself (no loop is left).
func TestRetiredStreamsGoToThePoolAtClose951(t *testing.T) {
	p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }), newTestFrameWriter(), newTestResponseWriter())
	m := p.manager
	a := openPoolStream(t, p, 1)
	p.executeHandler(a)
	if a.manager == nil {
		t.Fatal("stream 1 was reset before the loop released it")
	}
	late := openPoolStream(t, p, 3) // its handler returns after Close
	m.Close()
	if a.manager != nil {
		t.Fatal("Close did not release the retired stream")
	}
	p.executeHandler(late)
	if late.manager != nil {
		t.Fatal("a handler that returned after Close left its stream unreleased")
	}
	if m.StreamCount() != 0 {
		t.Fatalf("after Close: %d streams in the map", m.StreamCount())
	}
}

// TestRetireDrainCloseDoNotDeadlock951 hammers the three that take the
// manager's lock and the streams' locks with OutboundPending, which takes
// them in the other nesting (the manager's, then each stream's): many pool
// handlers retiring streams that hold buffered bytes, the loop draining, and
// OutboundPending and StreamCount polling, then Close. It must finish.
func TestRetireDrainCloseDoNotDeadlock951(t *testing.T) {
	p := NewProcessor(HandlerFunc(func(_ context.Context, s *Stream) error {
		s.SetHeadersSent()
		s.BufferOutbound(make([]byte, 64), true)
		return nil
	}), newTestFrameWriter(), newTestResponseWriter())
	m := p.manager
	m.SetMaxConcurrentStreams(1 << 20)
	const workers, per = 8, 2000
	stop := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(2)
	go func() { // the event loop's batch ends: the only one that drains
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				p.FlushInlineCleanup()
			}
		}
	}()
	go func() { // the shutdown's check, and a stream count
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = p.OutboundPending()
				_ = m.StreamCount()
			}
		}
	}()
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range per {
				id := uint32(2*(w*per+i) + 1)
				s, ok := m.TryOpenStream(id)
				if !ok {
					t.Errorf("TryOpenStream(%d) refused", id)
					return
				}
				s.SetState(StateHalfClosedRemote)
				s.flags.Or(flagAsyncRunning)
				p.poolRunning.Add(1)
				s.SetWindowSize(0)
				// Half of them are reset by the peer while their handler runs.
				if i%2 == 0 {
					go m.resetStream(id)
				}
				p.executeHandler(s)
			}
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(stop); bg.Wait(); m.Close(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(60 * time.Second):
		buf := make([]byte, 1<<20)
		t.Fatalf("deadlock: the pool handlers, the loop's drain, OutboundPending and Close did not finish in 60s\n%s", buf[:runtime.Stack(buf, true)])
	}
	if got := m.OutboundHeld(); got != 0 {
		t.Fatalf("every stream is released and the connection closed, and %d bytes are still charged to its outbound budget", got)
	}
}

// TestRetiredStreamDropsItsRequest951 checks that a pool stream does not hold
// its request while it waits for the event loop to release it: a connection
// that goes quiet after its async requests would otherwise keep their headers
// (up to the header list limit each, for as many streams as it opened) until
// it closes. A detached Context that reads the stream meanwhile sees nothing.
func TestRetiredStreamDropsItsRequest951(t *testing.T) {
	for _, detached := range []bool{false, true} {
		name := "plain"
		if detached {
			name = "detached"
		}
		t.Run(name, func(t *testing.T) {
			p := NewProcessor(HandlerFunc(func(_ context.Context, s *Stream) error {
				if detached {
					s.MarkDetached()
				}
				return nil
			}), newTestFrameWriter(), newTestResponseWriter())
			s := openPoolStream(t, p, 1)
			big := string(make([]byte, 64<<10))
			s.AddHeadersBatch([][2]string{{":method", "GET"}, {":path", "/"}, {"x-big", big}})
			s.Trailers = append(s.Trailers, [2]string{"x-trailer", big})
			s.SetRawBody(make([]byte, 1<<10))
			p.executeHandler(s)
			if s.manager == nil {
				t.Fatal("the stream was released by its handler's goroutine")
			}
			if n := len(s.GetHeaders()); n != 0 {
				t.Fatalf("the retired stream still holds %d request headers", n)
			}
			for i, h := range s.hdrBuf {
				if h != ([2]string{}) {
					t.Fatalf("hdrBuf[%d] still references %d bytes of header", i, len(h[0])+len(h[1]))
				}
			}
			if len(s.Trailers) != 0 || cap(s.Trailers) > 0 && s.Trailers[:1][0] != ([2]string{}) {
				t.Fatalf("the retired stream still holds its trailers: %v", s.Trailers)
			}
			if s.rawBody != nil {
				t.Fatalf("the retired stream still holds a %d-byte body view", len(s.rawBody))
			}
			p.FlushInlineCleanup()
		})
	}
}
