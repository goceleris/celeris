//go:build linux

package epoll

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#936: an async-route Hijack closes the original descriptor from the
// conn's dispatch goroutine, while the loop thread is still inside drainRead
// for the same conn. drainRead hands a read to the goroutine and then goes
// round again to read the rest of the edge, so its next read(2) can run after
// the goroutine has hijacked and closed the descriptor, and by then the kernel
// may have reissued the number. fd numbers are the process's, not the
// loop's: the number can be a client socket, another worker's conn or a
// driver's, and the loop thread then reads that socket's bytes into the
// hijacked conn's receive buffer. The owner never sees them: a request that
// was consumed and never answered, 1 in a few hundred runs of
// TestHijackKeepsRequestViews/epoll-async under `-race` and CPU load, where
// the reissued number is the test client's own socket for the next connection
// and the stolen bytes are its response.
//
// The tests make the windows deterministic. TestHijackedDescriptorIsNotRead
// AfterHandoff936: testHookAsyncHandoff runs on the loop thread right after
// the hand-off; it waits for the handler's Hijack to return (the descriptor is
// closed by then), puts a socket with a marker in it on the freed number, and
// lets the loop go round. Whatever the loop does next, it must not read that
// socket. That test pins that a check exists. Two more pin how it is made:
// the check and the read are one critical section (TestHijackedDescriptor
// ReadIsAtomicWithTheCheck936, a seam between them), and hijacked is stored
// under asyncInMu before the descriptor is touched (TestHijackMarksBefore
// Detach936). A fourth is the write side of the same class: a read that ended
// the conn (EOF) just before the hijack must not flush the reply an earlier
// pipelined request left in writeBuf to the reissued number
// (TestHijackedDescriptorIsNotWrittenAfterReadEnd936).

// reread936Handler: /hj hijacks (on the dispatch goroutine, the route being
// async) and closes the hijacked conn, /async is an async route that
// answers normally, /w answers inline with the worker's id, anything else
// answers inline. With gate set, /hj waits for it to be closed before it
// hijacks.
type reread936Handler struct {
	hijacked chan struct{} // closed when the first Hijack has returned
	gate     chan struct{} // nil: /hj hijacks at once
	once     atomic.Bool
}

func newReread936Handler(gated bool) *reread936Handler {
	h := &reread936Handler{hijacked: make(chan struct{})}
	if gated {
		h.gate = make(chan struct{})
	}
	return h
}

func (h *reread936Handler) HandleStream(ctx context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	if s.Path == "/hj" {
		hj, ok := s.ResponseWriter.(stream.Hijacker)
		if !ok {
			return fmt.Errorf("response writer %T cannot hijack", s.ResponseWriter)
		}
		if h.gate != nil {
			select {
			case <-h.gate:
			case <-time.After(10 * time.Second):
				return fmt.Errorf("the gate never opened")
			}
		}
		c, err := hj.Hijack(s)
		if err != nil {
			return err
		}
		if h.once.CompareAndSwap(false, true) {
			close(h.hijacked)
		}
		return c.Close()
	}
	body := []byte("ok")
	if s.Path == "/w" {
		id, _ := ctxkit.WorkerIDFrom(ctx)
		body = []byte(strconv.Itoa(id))
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(body))}}, body)
}

func (h *reread936Handler) RouteAsync(_, path string) bool { return path == "/hj" || path == "/async" }
func (h *reread936Handler) HasAsyncRoutes() bool           { return true }

const reread936Marker = "BYTES-OF-ANOTHER-SOCKET"

// reissue936 does what the kernel does for the next accept or connect: puts a
// new socket on the descriptor number fd, which Hijack has closed, and writes
// the marker into it. ask is the socket on fd, peer its other end, which stays
// open (closing it would turn the marker into an EOF). F_DUPFD returns the
// lowest free number at or above fd, so it never replaces a descriptor that
// something else took after the caller's check: if the number is not fd, the
// number was taken and the iteration fails.
func reissue936(fd int) (ask, peer int, err error) {
	if _, e := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); e != unix.EBADF {
		return -1, -1, fmt.Errorf("descriptor %d is still open after Hijack returned (err %v)", fd, e)
	}
	pair, e := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if e != nil {
		return -1, -1, e
	}
	if pair[0] == fd || pair[1] == fd {
		ask, peer = pair[0], pair[1]
		if peer == fd {
			ask, peer = peer, ask
		}
	} else {
		n, e := unix.FcntlInt(uintptr(pair[0]), unix.F_DUPFD_CLOEXEC, fd)
		_ = unix.Close(pair[0])
		if e != nil {
			_ = unix.Close(pair[1])
			return -1, -1, fmt.Errorf("moving a socket onto %d: %w", fd, e)
		}
		if n != fd {
			_ = unix.Close(n)
			_ = unix.Close(pair[1])
			return -1, -1, fmt.Errorf("descriptor %d was taken by something else before it could be reissued (got %d)", fd, n)
		}
		ask, peer = n, pair[1]
	}
	if _, e := unix.Write(peer, []byte(reread936Marker)); e != nil {
		_ = unix.Close(ask)
		_ = unix.Close(peer)
		return -1, -1, e
	}
	return ask, peer, nil
}

// outcome936 is what a hook did to the descriptor it was given.
type outcome936 struct {
	fd       int  // the old descriptor number
	ask      int  // the reissued socket, on that number; -1 if none
	peer     int  // its other end; -1 if none
	worker   int  // the loop that ran the hook
	reissued bool // the hook found Hijack done and reissued the number
	err      error
}

func (o outcome936) closeFDs() {
	if o.ask >= 0 {
		_ = unix.Close(o.ask)
	}
	if o.peer >= 0 {
		_ = unix.Close(o.peer)
	}
}

// waitServedBy936 returns once a request that loop `worker` serves has been
// answered, i.e. the loop has left whatever it was in. (Redial until a
// connection lands on that loop: the worker is the kernel's pick.)
func waitServedBy936(t *testing.T, addr string, worker int) {
	t.Helper()
	for try := 0; try < 64; try++ {
		got, err := inlineHijackRequest(addr, "GET /w HTTP/1.1\r\nHost: x\r\n\r\n")
		if err != nil {
			t.Fatalf("request after the hijack: %v", err)
		}
		if got == strconv.Itoa(worker) {
			return
		}
	}
	t.Fatalf("64 connections, none served by worker %d", worker)
}

func TestHijackedDescriptorIsNotReadAfterHandoff936(t *testing.T) {
	for _, tc := range []struct {
		name string
		// promote sends a request to /async first, so the conn is already
		// promoted when the /hj request arrives and the hand-off is the
		// promoted-conn branch of drainRead, not the promoting request's.
		promote bool
	}{
		{"promoting-request", false},
		{"promoted-conn", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newReread936Handler(false)
			_, addr, stop := startInlineHijackEngine(t, 2, h)

			// The hook is for the hand-off of the /hj request: the first one when
			// that request promotes the conn, the second when a /async request
			// promoted it before. Counted, not gated on the client having seen
			// the /async response: the loop thread calls the hook after the
			// hand-off, the dispatch goroutine answers meanwhile, and a loop
			// thread that was not run for a while (a loaded machine, -race,
			// -cover) calls the /async hand-off's hook after the client has the
			// response, which a flag set on the response took for the /hj hand-off:
			// the hook then waited 5 s for a Hijack that the parked loop thread
			// never read the request for ("the handler never returned from
			// Hijack", 5 of 600 runs on six CPU hogs; celeris#1104).
			var handoffs atomic.Int32
			want := int32(1)
			if tc.promote {
				want = 2
			}
			asyncAnswered := make(chan struct{}) // closed once the client has the /async response
			done := make(chan outcome936, 1)
			hook := func(fd int, cs *connState) {
				n := handoffs.Add(1)
				if tc.promote && n == 1 {
					// That late call, on every run: the loop thread is held
					// here until the client has the /async response.
					select {
					case <-asyncAnswered:
					case <-time.After(5 * time.Second):
					}
				}
				if n != want { // the count is the one-shot: n == want holds for one call
					return
				}
				o := outcome936{fd: fd, ask: -1, peer: -1, worker: -1}
				o.worker, _ = ctxkit.WorkerIDFrom(cs.ctx)
				defer func() { done <- o }()
				select {
				case <-h.hijacked:
				case <-time.After(5 * time.Second):
					o.err = fmt.Errorf("the handler never returned from Hijack")
					return
				}
				// Hijack closed fd before it returned.
				o.ask, o.peer, o.err = reissue936(fd)
				o.reissued = o.err == nil
			}
			testHookAsyncHandoff.Store(&hook)
			t.Cleanup(func() { testHookAsyncHandoff.Store(nil) })

			c, err := net.DialTimeout("tcp", addr, 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			br := bufio.NewReader(c)
			if tc.promote {
				if _, err := io.WriteString(c, "GET /async HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
					t.Fatal(err)
				}
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatalf("GET /async: %v", err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				close(asyncAnswered)
			}
			if _, err := io.WriteString(c, "GET /hj HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
				t.Fatal(err)
			}

			var o outcome936
			select {
			case o = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the hand-off hook never ran")
			}
			defer o.closeFDs()
			if o.err != nil {
				t.Fatal(o.err)
			}

			// The hijacked conn's handler closes it: the client sees EOF.
			if _, err := br.ReadByte(); err != io.EOF {
				t.Fatalf("hijacked conn: read = %v, want EOF", err)
			}
			waitServedBy936(t, addr, o.worker)
			assertMarkerIntact936(t, o)
			if !stop() {
				t.Error("the engine did not stop")
			}
		})
	}
}

// assertMarkerIntact936 reads the reissued socket: the marker must still be in
// it, unread by the engine.
func assertMarkerIntact936(t *testing.T, o outcome936) {
	t.Helper()
	buf := make([]byte, 64)
	n, err := unix.Read(o.ask, buf)
	switch {
	case err == unix.EAGAIN:
		t.Errorf("the loop thread read descriptor %d after the conn that had it was hijacked and the number was reissued: the marker %q of the new owner is gone (celeris#936)", o.fd, reread936Marker)
	case err != nil:
		t.Errorf("read of the reissued descriptor %d: %v", o.fd, err)
	case string(buf[:n]) != reread936Marker:
		t.Errorf("reissued descriptor %d holds %q, want %q", o.fd, buf[:n], reread936Marker)
	}
}

// TestHijackedDescriptorReadIsAtomicWithTheCheck936 pins that readPromoted's
// hijacked check and its read(2) are one critical section against Hijack. The
// hook sits between them, inside the section, and lets the handler hijack. If
// the hijack gets through (no lock, or a lock that does not cover the store),
// it has closed the descriptor by the time the hook looks, and the hook
// reissues the number to a marker socket, which the read that follows would
// consume. With the lock, the hijack waits for the section to end, the hook
// sees no hijack within its window and reissues nothing, and the read is of the
// conn's own socket.
func TestHijackedDescriptorReadIsAtomicWithTheCheck936(t *testing.T) {
	h := newReread936Handler(true)
	_, addr, stop := startInlineHijackEngine(t, 2, h)

	var fired atomic.Bool
	done := make(chan outcome936, 1)
	hook := func(fd int, cs *connState) {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		o := outcome936{fd: fd, ask: -1, peer: -1, worker: -1}
		o.worker, _ = ctxkit.WorkerIDFrom(cs.ctx)
		defer func() { done <- o }()
		close(h.gate) // the handler may hijack now
		select {
		case <-h.hijacked:
			// The hijack ran inside the critical section.
			o.ask, o.peer, o.err = reissue936(fd)
			o.reissued = o.err == nil
		case <-time.After(500 * time.Millisecond):
			// Held off by the lock, as it must be.
		}
	}
	testHookReadPromoted.Store(&hook)
	t.Cleanup(func() { testHookReadPromoted.Store(nil) })

	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, "GET /hj HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	var o outcome936
	select {
	case o = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the read hook never ran")
	}
	defer o.closeFDs()
	if o.err != nil {
		t.Fatal(o.err)
	}
	if _, err := bufio.NewReader(c).ReadByte(); err != io.EOF {
		t.Fatalf("hijacked conn: read = %v, want EOF", err)
	}
	waitServedBy936(t, addr, o.worker)
	if o.reissued {
		assertMarkerIntact936(t, o)
		t.Errorf("Hijack got through while readPromoted held the conn between its check and its read (celeris#936)")
	}
	if !stop() {
		t.Error("the engine did not stop")
	}
}

// TestHijackMarksBeforeDetach936 pins the order inside hijackConn: hijacked is
// stored (under asyncInMu) before the descriptor leaves epoll, so a loop-thread
// read that takes the lock after that point sees it, and one that took it
// before has finished.
func TestHijackMarksBeforeDetach936(t *testing.T) {
	h := newReread936Handler(false)
	_, addr, stop := startInlineHijackEngine(t, 2, h)

	var calls atomic.Int64
	var marked atomic.Int64
	hook := func(cs *connState) {
		calls.Add(1)
		if cs.hijacked.Load() {
			marked.Add(1)
		}
	}
	testHookHijackMarked.Store(&hook)
	t.Cleanup(func() { testHookHijackMarked.Store(nil) })

	if got, err := inlineHijackRequest(addr, "GET /hj HTTP/1.1\r\nHost: x\r\n\r\n"); err == nil {
		t.Fatalf("hijacked conn answered %q", got)
	}
	select {
	case <-h.hijacked:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never returned from Hijack")
	}
	if calls.Load() != 1 {
		t.Fatalf("off-thread hijack hook ran %d times, want 1 (the /hj route is async)", calls.Load())
	}
	if marked.Load() != 1 {
		t.Errorf("hijacked was not stored before the descriptor left epoll (celeris#936)")
	}
	if !stop() {
		t.Error("the engine did not stop")
	}
}

// TestHijackedDescriptorIsNotWrittenAfterReadEnd936 is the write side of the
// same class. A pipelined request ahead of the hijacking one is answered inline
// and its reply waits in cs.writeBuf; the hijack does not flush or clear it. The
// loop thread's read of the conn meets EOF (the client half-closed) under the
// lock, before the hijack; then the handler hijacks, returns and drops
// detachMu, and the descriptor number is reissued; and only then closeOnReadEnd
// takes the lock. It used to flush writeBuf to cs.fd and call OnError on a conn
// the application owns: the reply landed on the number's new owner. The hook
// stands at the start of closeOnReadEnd and plays the preemption.
func TestHijackedDescriptorIsNotWrittenAfterReadEnd936(t *testing.T) {
	h := newReread936Handler(true)
	_, addr, stop := startInlineHijackEngine(t, 2, h)

	var fired atomic.Bool
	done := make(chan outcome936, 1)
	hook := func(fd int, cs *connState) {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		o := outcome936{fd: fd, ask: -1, peer: -1, worker: -1}
		o.worker, _ = ctxkit.WorkerIDFrom(cs.ctx)
		defer func() { done <- o }()
		close(h.gate)
		select {
		case <-h.hijacked:
		case <-time.After(5 * time.Second):
			o.err = fmt.Errorf("the handler never returned from Hijack")
			return
		}
		// Wait for the goroutine to be gone (it drops detachMu on its way out),
		// so closeOnReadEnd takes the lock and does not hand the close to it.
		for dl := time.Now().Add(5 * time.Second); ; {
			cs.asyncInMu.Lock()
			running := cs.asyncRun
			cs.asyncInMu.Unlock()
			if !running {
				break
			}
			if time.Now().After(dl) {
				o.err = fmt.Errorf("the dispatch goroutine never exited")
				return
			}
			time.Sleep(time.Millisecond)
		}
		o.ask, o.peer, o.err = reissue936(fd)
		o.reissued = o.err == nil
	}
	testHookCloseOnReadEnd.Store(&hook)
	t.Cleanup(func() { testHookCloseOnReadEnd.Store(nil) })

	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	// /ok is answered inline and its reply stays in writeBuf; /hj promotes the
	// conn. The FIN follows at once.
	if _, err := io.WriteString(c, "GET /ok HTTP/1.1\r\nHost: x\r\n\r\nGET /hj HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	var o outcome936
	select {
	case o = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("closeOnReadEnd never ran")
	}
	defer o.closeFDs()
	if o.err != nil {
		t.Fatal(o.err)
	}
	waitServedBy936(t, addr, o.worker)

	// The reissued number's owner must have been left alone: only its own
	// marker is in its peer's direction (nothing was written to it).
	buf := make([]byte, 512)
	n, err := unix.Read(o.peer, buf)
	switch {
	case err == unix.EAGAIN:
	case err != nil:
		t.Errorf("read of the peer of the reissued descriptor %d: %v", o.fd, err)
	default:
		t.Errorf("the engine wrote %d bytes to descriptor %d after the conn that had it was hijacked and the number was reissued: %q (celeris#936)", n, o.fd, buf[:n])
	}
	if !stop() {
		t.Error("the engine did not stop")
	}
}
