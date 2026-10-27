//go:build linux

package iouring

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#1104, io_uring twin: the gate's pass (closeIdleH1) runs at the top of
// a worker iteration, before the iteration reaps the completions posted since
// the last reap. A keep-alive conn whose next request has reached the kernel
// (a recv CQE not yet reaped, or the bytes still in the socket) looks idle: it
// has detectAccum empty and nothing queued. A handler running inline on the
// worker thread holds the iteration, as epoll's test does, and runs the pass
// the way the loop does once the gate is up.

type gateUnreadIOU1104 struct {
	eng     atomic.Pointer[Engine]
	started chan struct{}
	release chan struct{}
	self    atomic.Pointer[string]
	pass    atomic.Int64
	diag    atomic.Value
	held    atomic.Int64 // conns of the held worker besides the handler's own, at the pass
}

func (h *gateUnreadIOU1104) HandleStream(ctx context.Context, s *stream.Stream) error {
	var path string
	for _, hd := range s.GetHeaders() {
		if hd[0] == ":path" {
			path = hd[1]
		}
	}
	if path == "/gate" {
		id, _ := ctxkit.WorkerIDFrom(ctx)
		w := h.eng.Load().workers[id]
		h.started <- struct{}{}
		<-h.release
		var self *connState
		for _, fd := range w.liveConns {
			if cs := w.conns[fd]; cs != nil && cs.remoteAddr == *h.self.Load() {
				self = cs
			}
		}
		if self != nil {
			self.closing = true // not the pass's to look at: it is mid-request
		}
		inq, fixed := 0, 0
		for _, fd := range w.liveConns {
			cs := w.conns[fd]
			if cs == nil || cs == self {
				continue
			}
			if !ownsDescriptorNumber(cs) {
				fixed++
				continue
			}
			if q, err := unix.IoctlGetInt(cs.fd, unix.TIOCINQ); err == nil && q > 0 {
				inq++
			}
		}
		cqh, cqt := w.ring.BeginCQ()
		h.diag.Store("live=" + strconv.Itoa(len(w.liveConns)) + " socket-queue-nonempty=" + strconv.Itoa(inq) + " fixed-file=" + strconv.Itoa(fixed) + " cq-unreaped=" + strconv.Itoa(int(cqt-cqh)))
		h.held.Store(int64(len(w.liveConns) - 1))
		w.refuseRecv = true
		w.closeIdleH1()
		h.pass.Add(1)
		if self != nil {
			self.closing = false
		}
	}
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", strconv.Itoa(len(path))}}, []byte(path))
}
func (*gateUnreadIOU1104) RouteAsync(_, _ string) bool { return false }
func (*gateUnreadIOU1104) HasAsyncRoutes() bool        { return false }

// TestGateDoesNotCloseAKeepAliveConnWithAnUnreadRequest1104: conns that have
// been served and are idle (decided HTTP/1), whose next request is in the
// kernel when the pass runs.
func TestGateDoesNotCloseAKeepAliveConnWithAnUnreadRequest1104(t *testing.T) {
	runGateUnread1104(t, false)
}

// TestGateDoesNotCloseAConnWithAnUnreadFirstRequest1104: conns accepted with no
// byte sent (TCP_DEFER_ACCEPT off, so the worker has them), whose first request
// is in the kernel when the pass runs: undecided conns the pass took for "sent
// nothing".
func TestGateDoesNotCloseAConnWithAnUnreadFirstRequest1104(t *testing.T) {
	runGateUnread1104(t, true)
}

func runGateUnread1104(t *testing.T, silent bool) {
	h := &gateUnreadIOU1104{started: make(chan struct{}, 1), release: make(chan struct{})}
	e := startGateUnread1104(t, h, silent)
	h.eng.Store(e)
	addr := e.Addr().String()
	type client struct {
		c  net.Conn
		br *bufio.Reader
	}
	read := func(cl client) (*http.Response, error) {
		_ = cl.c.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(cl.br, nil)
		if err != nil {
			return nil, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp, nil
	}
	const n = 32
	var clients []client
	for i := 0; i < n; i++ {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		cl := client{c, bufio.NewReader(c)}
		if !silent {
			if _, err := c.Write([]byte("GET /x HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			if _, err := read(cl); err != nil {
				t.Fatalf("first %d: %v", i, err)
			}
		}
		clients = append(clients, cl)
	}
	// The worker has accepted the silent conns, and armed their recv, by now.
	time.Sleep(300 * time.Millisecond)
	b, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	self := b.LocalAddr().String()
	h.self.Store(&self)
	if _, err := b.Write([]byte("GET /gate HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no /gate")
	}
	for i, cl := range clients {
		if _, err := cl.c.Write([]byte("GET /y HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("second %d: %v", i, err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	close(h.release)
	closed, answered, none, closeAfter := 0, 0, 0, 0
	var first error
	for _, cl := range clients {
		resp, err := read(cl)
		var ne net.Error
		switch {
		case err == nil:
			answered++
			if resp.Close {
				closeAfter++
			}
		case errors.As(err, &ne) && ne.Timeout():
			none++
		default:
			closed++
			if first == nil {
				first = err
			}
		}
	}
	t.Logf("before the pass: %v", h.diag.Load())
	t.Logf("pass ran=%d; of %d clients with a request in flight: %d answered, %d closed (first: %v reset=%v), %d no answer", h.pass.Load(), n, answered, closed, first, errors.Is(first, syscall.ECONNRESET), none)
	t.Logf("%d answers carry Connection: close; the held worker had %d conns", closeAfter, h.held.Load())
	if closed != 0 {
		t.Errorf("the pass closed %d conns whose request had reached the kernel (celeris#1104)", closed)
	}
	if int64(closeAfter) != h.held.Load() {
		t.Errorf("%d answers carry Connection: close, want %d (every conn of the held worker: a request read after the gate is answered close-after)", closeAfter, h.held.Load())
	}
}

// startGateUnread1104 is start543 with TCP_DEFER_ACCEPT off when silent: the
// conns are then accepted at their connect, before they send a byte.
func startGateUnread1104(t *testing.T, h stream.Handler, silent bool) *Engine {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, cancel, done := startRingRetried662(t, func() (*Engine, error) {
		return New(resource.Config{
			Addr: addr, Protocol: engine.Auto, DisableDeferAccept: silent,
			Resources: resource.Resources{Workers: 2},
		}, h)
	})
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	return e
}

// armPostedRecv1104 arms a recv on cs and lets the kernel complete it: peer
// writes req, the SQE is submitted, and the call returns once the CQE is in the
// completion queue, UNREAPED (no loop runs here). This is the shape of a request
// on a ring whose recv completions are posted without the worker's help (the
// base tier, and any ring where the kernel posts at once): the bytes have left
// the socket's queue, and are a CQE the gate's pass, at the top of the worker
// iteration, runs ahead of.
func armPostedRecv1104(t *testing.T, w *Worker, cs *connState, peer int, req string) {
	t.Helper()
	sqe := w.ring.GetSQE()
	if sqe == nil {
		t.Fatal("no SQE")
	}
	prepRecv(sqe, cs.fd, cs.buf)
	setSQEUserData(sqe, encodeUserDataGen(udRecv, cs.fd, cs.generation))
	cs.recvArmed = true
	cs.kernelInflight++
	if req != "" {
		if _, err := unix.Write(peer, []byte(req)); err != nil {
			t.Fatalf("write: %v", err)
		}
	} else if err := unix.Shutdown(peer, unix.SHUT_WR); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := w.ring.SubmitAndWait(); err != nil {
		t.Fatalf("SubmitAndWait: %v", err)
	}
	if head, tail := w.ring.BeginCQ(); head == tail {
		t.Fatal("premise: no CQE posted")
	}
}

// TestGatePassHoldsAConnWithARecvCQEPosted1104: a conn whose recv completion is
// already in the completion queue holds a request (the base tier's shape, in
// which the CI run showed 18 conns closed with their request delivered by the
// kernel and not yet reaped): the pass must not close it, and a conn that is
// idle (control) is still closed.
func TestGatePassHoldsAConnWithARecvCQEPosted1104(t *testing.T) {
	w, css, peers := gateRig884(t, 2)
	armPostedRecv1104(t, w, css[0], peers[0], "GET /x HTTP/1.1\r\nHost: x\r\n\r\n")
	w.refuseRecv = true
	w.closeIdleH1()
	if w.conns[css[0].fd] != css[0] {
		t.Errorf("the pass closed a conn whose request is a posted recv CQE (celeris#1104)")
	}
	if !css[0].gateRead {
		t.Errorf("the conn with a posted recv CQE is not marked to be read on")
	}
	if w.conns[css[1].fd] == css[1] {
		t.Errorf("the idle control conn is still open: the pass must still close what holds nothing")
	}
}

// TestGatePassHoldsAConnWithBytesInTheSocketQueue1104 is the other shape, on a
// ring whose completions are task work (COOP_TASKRUN, DEFER_TASKRUN): the request
// waits in the socket's queue, with no CQE yet.
func TestGatePassHoldsAConnWithBytesInTheSocketQueue1104(t *testing.T) {
	w, css, peers := gateRig884(t, 2)
	if _, err := unix.Write(peers[0], []byte("GET /x HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	w.refuseRecv = true
	w.closeIdleH1()
	if w.conns[css[0].fd] != css[0] {
		t.Errorf("the pass closed a conn whose request is in the socket's queue (celeris#1104)")
	}
	if !css[0].gateRead {
		t.Errorf("the conn with bytes in its socket queue is not marked to be read on")
	}
	if w.conns[css[1].fd] == css[1] {
		t.Errorf("the idle control conn is still open")
	}
}

// TestGatePassClosesAConnWhoseRecvCQEIsAnEOF1104: a posted recv CQE with no
// bytes (the peer's FIN) is not a request: the conn is closed.
func TestGatePassClosesAConnWhoseRecvCQEIsAnEOF1104(t *testing.T) {
	w, css, peers := gateRig884(t, 1)
	armPostedRecv1104(t, w, css[0], peers[0], "")
	w.refuseRecv = true
	w.closeIdleH1()
	if w.conns[css[0].fd] == css[0] {
		t.Errorf("the pass kept a conn whose posted recv CQE is an EOF")
	}
}

// TestGatePassIgnoresAPostedRecvCQEOfAnotherGeneration1104: the CQE names a
// conn that has closed since and whose number another conn holds (a different
// generation): it is not the live conn's request, and the live conn, idle, is
// closed.
func TestGatePassIgnoresAPostedRecvCQEOfAnotherGeneration1104(t *testing.T) {
	w, css, peers := gateRig884(t, 1)
	armPostedRecv1104(t, w, css[0], peers[0], "GET /x HTTP/1.1\r\nHost: x\r\n\r\n")
	css[0].generation++
	w.refuseRecv = true
	w.closeIdleH1()
	if w.conns[css[0].fd] == css[0] {
		t.Errorf("the pass kept a conn on the strength of a CQE of another generation")
	}
}
