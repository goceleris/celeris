package conn

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// TestLateCallsLeaveTheStreamAndTheBudgetAlone904: through the UseStreamer API a
// StreamWriter uses, with the token it took while its handler ran. After the
// handler returned every call is refused and changes nothing, on the sync route
// both when the stream was released at once and when it stayed for a buffered
// rest (the use ends at the handler's return, not at the release).
func TestLateCallsLeaveTheStreamAndTheBudgetAlone904(t *testing.T) {
	for _, tc := range []struct {
		name    string
		settled int // initial stream window: 1000 keeps a rest buffered, 0 means the default (nothing buffered)
	}{{"released", 0}, {"kept-for-a-buffered-rest", 1000}} {
		t.Run(tc.name, func(t *testing.T) {
			var sw stream.UseStreamer
			var s *stream.Stream
			var gen uint64
			h := &syncHandler893{run: func(_ context.Context, st *stream.Stream) error {
				u := st.ResponseWriter.(stream.UseStreamer)
				if err := u.WriteHeaderUse(st, st.Gen(), 200, nil); err != nil {
					return err
				}
				if err := u.WriteUse(st, st.Gen(), make([]byte, 3000)); err != nil {
					return err
				}
				sw, s, gen = u, st, st.Gen()
				return nil
			}}
			c := newFlushConn(t, h, H2Config{})
			var settings []http2.Setting
			if tc.settled != 0 {
				settings = []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: uint32(tc.settled)}}
			}
			c.open(0, settings, 1)
			c.drain()
			m := c.st.processor.GetManager()
			held, wire := m.OutboundHeld(), len(c.frames())

			for _, call := range []struct {
				name string
				err  error
			}{
				{"WriteUse", sw.WriteUse(s, gen, []byte("late"))},
				{"CloseUse", sw.CloseUse(s, gen)},
				{"WriteHeaderUse", sw.WriteHeaderUse(s, gen, 200, nil)},
			} {
				if !errors.Is(call.err, stream.ErrStreamEnded) {
					t.Errorf("late %s returned %v, want stream.ErrStreamEnded", call.name, call.err)
				}
			}
			c.drain()
			if got := m.OutboundHeld(); got != held {
				t.Errorf("late calls moved the budget from %d to %d", held, got)
			}
			if got := len(c.frames()); got != wire {
				t.Errorf("late calls put %d frame(s) on the wire", got-wire)
			}
			if tc.settled != 0 {
				// The buffered rest is still flushed by the peer's grant, whole,
				// and the late bytes are not in it.
				c.grant(1<<20, true, 1)
				c.drain()
				if got := len(c.stream(1).data); got != 3000 {
					t.Errorf("%d DATA bytes on the wire after the grant, want the handler's 3000", got)
				}
			}
		})
	}
}

// TestLateTimeoutResetLeavesTheNextUseAlone904: a pool Write whose wait ends
// at WriteTimeout resets its stream. If the stream's use ended meanwhile (the
// handler returned and the object may serve another stream), that reset must
// neither queue a RST_STREAM nor cancel the object: it would reset and cancel
// an unrelated stream, on any connection.
func TestLateTimeoutResetLeavesTheNextUseAlone904(t *testing.T) {
	var s *stream.Stream
	var gen uint64
	h := &syncHandler893{run: func(_ context.Context, st *stream.Stream) error {
		s, gen = st, st.Gen()
		return st.ResponseWriter.(stream.Streamer).WriteHeader(st, 200, nil)
	}}
	c := newFlushConn(t, h, H2Config{})
	c.open(0, nil, 1)
	c.drain()
	// The handler returned and its stream was released: the object is in the pool.
	s.EndUse()
	before := len(c.frames())
	c.st.adapter.resetStreamUse(s, gen, http2.ErrCodeInternal)
	c.drain()
	if got := len(c.frames()); got != before {
		t.Errorf("a late reset queued %d frame(s)", got-before)
	}
	if s.IsCancelled() {
		t.Error("a late reset cancelled the stream of a later use")
	}
}
