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
// The test makes the window deterministic. testHookAsyncHandoff runs on the
// loop thread right after the hand-off; it waits for the handler's Hijack to
// return (the descriptor is closed by then), puts a socket with a marker in
// it on the freed number, and lets the loop go round. Whatever the loop does
// next, it must not read that socket.

// reread936Handler: /hj hijacks (on the dispatch goroutine, the route being
// async) and closes the hijacked conn, /async is an async route that
// answers normally, /w answers inline with the worker's id, anything else
// answers inline.
type reread936Handler struct {
	hijacked chan struct{} // closed when the first Hijack has returned
	once     atomic.Bool
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
			h := &reread936Handler{hijacked: make(chan struct{})}
			_, addr, stop := startInlineHijackEngine(t, 2, h)

			var armed atomic.Bool
			type outcome struct {
				fd     int // the old descriptor number
				ask    int // the reissued socket's end, on that number
				peer   int // its other end, which stays open: the marker is not an EOF
				worker int // the loop that made the hand-off
				err    error
			}
			done := make(chan outcome, 1)
			var fired atomic.Bool
			hook := func(fd int, cs *connState) {
				if !armed.Load() || !fired.CompareAndSwap(false, true) {
					return
				}
				o := outcome{fd: fd, ask: -1, peer: -1, worker: -1}
				o.worker, _ = ctxkit.WorkerIDFrom(cs.ctx)
				defer func() { done <- o }()
				select {
				case <-h.hijacked:
				case <-time.After(5 * time.Second):
					o.err = fmt.Errorf("the handler never returned from Hijack")
					return
				}
				// Hijack closed fd before it returned. Give the number to a
				// socket with bytes in it, as the kernel gives it to the next
				// accept or connect.
				if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != unix.EBADF {
					o.err = fmt.Errorf("descriptor %d is still open after Hijack returned (err %v)", fd, err)
					return
				}
				pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
				if err != nil {
					o.err = err
					return
				}
				// socketpair takes the lowest free numbers, normally fd itself;
				// if not, move one end onto it.
				mine, other := pair[0], pair[1]
				if other == fd {
					mine, other = other, mine
				}
				if mine != fd {
					if err := unix.Dup3(mine, fd, unix.O_CLOEXEC); err != nil {
						_ = unix.Close(mine)
						o.err = fmt.Errorf("dup3 onto %d: %w", fd, err)
						return
					}
					_ = unix.Close(mine)
				}
				o.peer = other
				if _, err := unix.Write(other, []byte(reread936Marker)); err != nil {
					_ = unix.Close(fd)
					_ = unix.Close(other)
					o.err = err
					return
				}
				o.ask = fd
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
			}
			armed.Store(true)
			if _, err := io.WriteString(c, "GET /hj HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
				t.Fatal(err)
			}

			var o outcome
			select {
			case o = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the hand-off hook never ran")
			}
			if o.err != nil {
				t.Fatal(o.err)
			}
			defer func() { _ = unix.Close(o.ask); _ = unix.Close(o.peer) }()

			// The hijacked conn's handler closes it: the client sees EOF.
			if _, err := br.ReadByte(); err != io.EOF {
				t.Fatalf("hijacked conn: read = %v, want EOF", err)
			}
			// A request that the hand-off's own loop serves is one it serves
			// after drainRead has returned, so whatever it was going to read of
			// the reissued number it has read by now. (Redial until a connection
			// lands on that loop: the worker is the kernel's pick.)
			served := false
			for try := 0; try < 64 && !served; try++ {
				got, err := inlineHijackRequest(addr, "GET /w HTTP/1.1\r\nHost: x\r\n\r\n")
				if err != nil {
					t.Fatalf("request after the hijack: %v", err)
				}
				served = got == strconv.Itoa(o.worker)
			}
			if !served {
				t.Fatalf("64 connections, none served by worker %d", o.worker)
			}

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
			if !stop() {
				t.Error("the engine did not stop")
			}
		})
	}
}
