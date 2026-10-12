package celeris

import (
	"net"
	"sync/atomic"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// hijackDetachWriter is a stream.ResponseWriter that can be hijacked and is
// told when a stream detaches, and counts both, so a test sees which of the
// engine's hooks Context.Detach reached.
type hijackDetachWriter struct {
	hijacks        atomic.Int32
	streamDetaches atomic.Int32
}

func (*hijackDetachWriter) WriteResponse(*stream.Stream, int, [][2]string, []byte) error {
	return nil
}

func (w *hijackDetachWriter) Hijack(*stream.Stream) (net.Conn, error) {
	w.hijacks.Add(1)
	a, b := net.Pipe()
	_ = b.Close()
	return a, nil
}

func (w *hijackDetachWriter) StreamDetached() { w.streamDetaches.Add(1) }

func newHijackDetachContext(t *testing.T) (*Context, *hijackDetachWriter, *atomic.Int32) {
	t.Helper()
	s := stream.NewStream(1)
	s.Headers = append(s.Headers,
		[2]string{":method", "GET"},
		[2]string{":path", "/"},
		[2]string{":scheme", "http"},
		[2]string{":authority", "localhost"},
	)
	w := &hijackDetachWriter{}
	s.ResponseWriter = w
	var onDetach atomic.Int32
	s.OnDetach = func() { onDetach.Add(1) }
	return acquireContext(s), w, &onDetach
}

// TestDetachAfterHijackIsLeftToTheCaller pins that Context.Detach after a
// successful Context.Hijack does not reach the engine: the connection is the
// caller's, and the engine has let go of its state for it. On epoll the
// OnDetach hook of a hijacked connection dereferenced the connection state the
// hijack had released and panicked (PR #1004 review, #889 item 35).
func TestDetachAfterHijackIsLeftToTheCaller(t *testing.T) {
	c, w, onDetach := newHijackDetachContext(t)
	conn, err := c.Hijack()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	done := c.Detach()
	if got := onDetach.Load(); got != 0 {
		t.Errorf("OnDetach ran %d times after a Hijack, want 0", got)
	}
	if got := w.streamDetaches.Load(); got != 0 {
		t.Errorf("StreamDetached ran %d times after a Hijack, want 0", got)
	}
	if !c.detached {
		t.Error("Detach after a Hijack must still keep the Context out of the pool until done is called")
	}
	done()
}

// TestDetachWithoutHijackStillReachesTheEngine is the control of the test
// above: the same Context and writer, no Hijack, and both hooks run once.
func TestDetachWithoutHijackStillReachesTheEngine(t *testing.T) {
	c, w, onDetach := newHijackDetachContext(t)
	done := c.Detach()
	if got := onDetach.Load(); got != 1 {
		t.Errorf("OnDetach ran %d times without a Hijack, want 1", got)
	}
	if got := w.streamDetaches.Load(); got != 1 {
		t.Errorf("StreamDetached ran %d times without a Hijack, want 1", got)
	}
	done()
}

// TestHijackedMarkDoesNotOutliveTheUse pins that a Context reused for the next
// request on its connection starts that request not hijacked: its Detach must
// reach the engine.
func TestHijackedMarkDoesNotOutliveTheUse(t *testing.T) {
	c, _, _ := newHijackDetachContext(t)
	conn, err := c.Hijack()
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	// A keep-alive H1 stream caches its Context: the release resets it and the
	// next request on the stream takes it again.
	c.stream.CachedCtx = c
	releaseContext(c)

	s2 := stream.NewStream(3)
	s2.Headers = append(s2.Headers,
		[2]string{":method", "GET"},
		[2]string{":path", "/"},
		[2]string{":scheme", "http"},
		[2]string{":authority", "localhost"},
	)
	w2 := &hijackDetachWriter{}
	s2.ResponseWriter = w2
	var onDetach atomic.Int32
	s2.OnDetach = func() { onDetach.Add(1) }
	s2.CachedCtx = c
	c2 := acquireContext(s2)
	if c2 != c {
		t.Fatal("the cached Context was not reused: the test does not exercise a reused Context")
	}
	done := c2.Detach()
	if got := onDetach.Load(); got != 1 {
		t.Errorf("OnDetach ran %d times on the reused Context, want 1: the hijacked mark outlived the request", got)
	}
	if got := w2.streamDetaches.Load(); got != 1 {
		t.Errorf("StreamDetached ran %d times on the reused Context, want 1", got)
	}
	done()
}
