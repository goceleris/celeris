package conn

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// celeris#884 item 82: ProcessH1 never returns ErrAsyncDispatch with a header
// deadline pending. Both of its async checks (the fresh-parse site and the
// buffered-path site) run after ClearHeaderDeadline, the head being complete.
// The io_uring engine relies on it: promoteConnToAsync used to read the
// deadline and arm the kernel timer from it, a branch no traffic reaches, and
// that branch is gone because of this invariant. The cases are the ways an
// inline ProcessH1 reaches an async route, each with a deadline armed the way
// the engine arms one (accept, and a keep-alive request's first bytes), so a
// change that moves either clear past its return fails here.
func TestInlineAsyncDispatchLeavesNoHeaderDeadline884(t *testing.T) {
	const (
		syncReq  = "GET /s HTTP/1.1\r\nHost: x\r\n\r\n"
		asyncReq = "GET /a HTTP/1.1\r\nHost: x\r\n\r\n"
	)
	newState := func() *H1State {
		s := NewH1State()
		s.InlineMode = true
		s.RouteAsync = func(_, p string) bool { return strings.HasPrefix(p, "/a") }
		s.ReadHeaderTimeoutNs = int64(10e9)
		s.ArmHeaderDeadline() // armed at accept
		if s.HeaderDeadlineNs.Load() == 0 {
			t.Fatal("apparatus: the deadline was not armed")
		}
		return s
	}
	h := &asyncDispatchHandler{}
	write := func([]byte) {}
	process := func(s *H1State, data string) error {
		return ProcessH1(context.Background(), []byte(data), s, h, write)
	}
	wantBail := func(t *testing.T, s *H1State, err error) {
		t.Helper()
		if !errors.Is(err, ErrAsyncDispatch) {
			t.Fatalf("apparatus: ProcessH1 = %v, want ErrAsyncDispatch", err)
		}
		if dl := s.HeaderDeadlineNs.Load(); dl != 0 {
			t.Errorf("celeris#884 item 82: ProcessH1 returned ErrAsyncDispatch with a header deadline pending (%d): "+
				"the promotion's arm is reachable again", dl)
		}
	}

	t.Run("fresh-head", func(t *testing.T) {
		s := newState()
		wantBail(t, s, process(s, asyncReq))
	})
	t.Run("sync-then-async-pipelined", func(t *testing.T) {
		s := newState()
		wantBail(t, s, process(s, syncReq+asyncReq))
	})
	t.Run("keep-alive-request-in-pieces", func(t *testing.T) {
		s := newState()
		if err := process(s, syncReq); err != nil {
			t.Fatalf("apparatus: %v", err)
		}
		if err := process(s, "GET /a HTTP/1.1\r\n"); err != nil {
			t.Fatalf("apparatus: %v", err)
		}
		if s.HeaderDeadlineNs.Load() == 0 {
			t.Fatal("apparatus: the partial head of a keep-alive request did not arm a deadline")
		}
		wantBail(t, s, process(s, "Host: x\r\n\r\n"))
	})
	t.Run("after-a-chunked-request", func(t *testing.T) {
		s := newState()
		chunked := "POST /s HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n3\r\nabc\r\n0\r\n\r\n"
		wantBail(t, s, process(s, chunked+asyncReq))
	})
	t.Run("after-a-direct-body", func(t *testing.T) {
		// The io_uring direct-body tail: the body's last bytes are received
		// into the body buffer, and what follows them goes back to ProcessH1.
		s := newState()
		post := func(path string, n int) string {
			return "POST " + path + " HTTP/1.1\r\nHost: x\r\nContent-Length: " + strconv.Itoa(n) + "\r\n\r\n"
		}
		// A large body first, so that bodyBuf has room for the next one.
		big := strings.Repeat("x", 20000)
		if err := process(s, post("/s", len(big))+big[:10000]); err != nil {
			t.Fatalf("apparatus: %v", err)
		}
		if err := process(s, big[10000:]); err != nil {
			t.Fatalf("apparatus: %v", err)
		}
		small := strings.Repeat("y", 2000)
		if err := process(s, post("/s", len(small))+small[:1000]); err != nil {
			t.Fatalf("apparatus: %v", err)
		}
		b := s.NextRecvBuf()
		rest := small[1000:] + asyncReq
		if b == nil || len(b) < len(rest) {
			t.Fatalf("apparatus: NextRecvBuf has room for %d bytes, want %d", len(b), len(rest))
		}
		copy(b, rest)
		if !s.ConsumeBodyRecv(len(rest)) {
			t.Fatal("apparatus: the body is not complete")
		}
		tail, verdict := s.DispatchBufferedBody(context.Background(), h, write)
		if verdict != nil || len(tail) != len(asyncReq) {
			t.Fatalf("apparatus: DispatchBufferedBody = %d bytes, %v", len(tail), verdict)
		}
		wantBail(t, s, ProcessH1(context.Background(), tail, s, h, write))
	})
}
