//go:build linux

package epoll

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#658. A standby loop with no listen socket and no connections parks
// on a Go channel that only ResumeAccept used to close. AdoptConn queues the
// descriptor, sets adoptQPending and writes the loop's eventfd — and returns
// nil, so the source has already relinquished the connection. A loop that is
// parked in a select is not in epoll_wait and never reads that eventfd, so the
// adoption waited for the next ResumeAccept. If this engine stayed the
// standby, it waited forever, and the loop's shutdown never looked at the
// queue: the descriptor stayed open, owned by no engine, in CLOSE_WAIT once
// the peer gave up. In the adaptive switch-vs-churn storm the leaked server
// sockets equal sum(TransplantDetached) - sum(TransplantAdopted) exactly.

// okHandler658 answers every request with a tiny 200, inline (no async
// routes), so the test exercises the adopt path and nothing else.
type okHandler658 struct{}

func (okHandler658) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}},
		[]byte("ok"))
}

// adoptPair658 returns a connected loopback TCP pair: the client end the test
// talks through, and a non-blocking dup of the server end that no Go poller
// owns — exactly the descriptor a source engine hands to AdoptConn.
func adoptPair658(t *testing.T) (net.Conn, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	srv, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	sc, err := srv.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatalf("syscallconn: %v", err)
	}
	fd, dupErr := -1, error(nil)
	if cerr := sc.Control(func(raw uintptr) { fd, dupErr = unix.Dup(int(raw)) }); cerr != nil {
		t.Fatalf("control: %v", cerr)
	}
	if dupErr != nil {
		t.Fatalf("dup: %v", dupErr)
	}
	_ = srv.Close()
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("setnonblock: %v", err)
	}
	return client, fd
}

// suspendedEpoll658 is an epoll engine that has become a standby: accept
// paused and every loop parked in DRAINING→SUSPENDED.
type suspendedEpoll658 struct {
	e      *Engine
	loops  []*Loop
	cancel context.CancelFunc
	errCh  chan error
}

func startSuspendedEpoll658(t *testing.T, onDisconnect func(string)) *suspendedEpoll658 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	e, err := New(resource.Config{
		Addr:         addr,
		Protocol:     engine.HTTP1,
		Resources:    resource.Resources{Workers: 2},
		OnDisconnect: onDisconnect,
	}, okHandler658{})
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &suspendedEpoll658{e: e, cancel: cancel, errCh: make(chan error, 1)}
	go func() { s.errCh <- e.Listen(ctx) }()
	t.Cleanup(func() { s.stop(t) })

	for dl := time.Now().Add(10 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(5 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	e.mu.Lock()
	s.loops = append([]*Loop(nil), e.loops...)
	e.mu.Unlock()
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); {
		if s.allSuspended() {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("loops never parked after PauseAccept (suspended=%v); the precondition "+
		"of this test is not reached", s.suspendedFlags())
	return nil
}

func (s *suspendedEpoll658) allSuspended() bool {
	for _, l := range s.loops {
		if !l.suspended.Load() {
			return false
		}
	}
	return len(s.loops) > 0
}

func (s *suspendedEpoll658) suspendedFlags() []bool {
	out := make([]bool, len(s.loops))
	for i, l := range s.loops {
		out[i] = l.suspended.Load()
	}
	return out
}

// stop cancels Listen and waits for it to return. Idempotent.
func (s *suspendedEpoll658) stop(t *testing.T) {
	t.Helper()
	s.cancel()
	if s.errCh == nil {
		return
	}
	select {
	case <-s.errCh:
		s.errCh = nil
	case <-time.After(5 * time.Second):
		t.Errorf("Listen did not return within 5s of cancel")
	}
}

// TestAdoptConnWakesASuspendedLoop hands a live descriptor to an engine whose
// loops are ALREADY parked. AdoptConn returns nil, so the source has let go:
// the adoption has to complete, and the connection has to be served, without
// anyone calling ResumeAccept.
func TestAdoptConnWakesASuspendedLoop(t *testing.T) {
	s := startSuspendedEpoll658(t, nil)
	client, fd := adoptPair658(t)

	if err := s.e.AdoptConn(fd, engine.Carryover{RemoteAddr: client.LocalAddr().String()}); err != nil {
		_ = unix.Close(fd)
		t.Fatalf("AdoptConn: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for s.e.Metrics().TransplantAdopted == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.e.Metrics().TransplantAdopted; got != 1 {
		pending := make([]int32, len(s.loops))
		for i, l := range s.loops {
			pending[i] = l.adoptQPending.Load()
		}
		t.Fatalf("TransplantAdopted = %d 2s after AdoptConn returned nil, want 1: the "+
			"descriptor is queued (adoptQPending=%v) on a loop parked on its wake "+
			"channel (suspended=%v), and the eventfd AdoptConn wrote cannot wake a "+
			"loop that is not in epoll_wait — the connection is owned by no engine "+
			"(celeris#658)", got, pending, s.suspendedFlags())
	}

	br := bufio.NewReader(client)
	for i := range 2 {
		_ = client.SetWriteDeadline(time.Now().Add(2 * time.Second))
		if _, err := client.Write([]byte("GET /x HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("write request %d: %v", i, err)
		}
		_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("request %d on the adopted conn got no response: %v", i, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: status %d, want 200", i, resp.StatusCode)
		}
	}
	if got := s.e.Metrics().ActiveConnections; got != 1 {
		t.Errorf("ActiveConnections = %d, want 1 — the adopted conn is live", got)
	}
}

// TestShutdownClosesAQueuedAdoption covers the other half: an adoption that is
// still in the queue when the loop shuts down. That is the state the lost
// wakeup leaves behind, and it is still reachable with the wakeup fixed — a
// loop woken by the kick checks its context at the top of the next iteration,
// before drainAdoptQueue — so the queue is built here by hand, without the
// wakeup, to pin the shutdown path on its own. The descriptor must be closed
// (the peer sees EOF) and the close accounted like any other refused adoption,
// because the source fired no OnDisconnect when it let go.
//
// Then, once the loop is gone, AdoptConn must refuse: a nil return hands the
// descriptor to a queue nothing will ever drain, while an error lets the
// source take the connection back through its existing reclaim path.
func TestShutdownClosesAQueuedAdoption(t *testing.T) {
	var hooks atomic.Int64
	s := startSuspendedEpoll658(t, func(string) { hooks.Add(1) })
	client, fd := adoptPair658(t)

	l := s.loops[0]
	l.adoptQMu.Lock()
	l.adoptQueue = append(l.adoptQueue, adoptItem{fd: fd, carry: engine.Carryover{RemoteAddr: "127.0.0.1:658"}})
	l.adoptQPending.Store(1)
	l.adoptQMu.Unlock()

	s.stop(t)

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := client.Read(make([]byte, 16))
	if !errors.Is(err, io.EOF) {
		t.Errorf("peer read = (%d, %v) after the engine shut down, want EOF: the "+
			"queued descriptor was never closed — Loop.shutdown does not look at the "+
			"adopt queue, so it stays open and owned by nobody (celeris#658)", n, err)
	}
	m := s.e.Metrics()
	if m.TransplantAdoptRefused != 1 || m.CloseCount != 1 || hooks.Load() != 1 {
		t.Errorf("TransplantAdoptRefused = %d, CloseCount = %d, OnDisconnect = %d, want "+
			"1, 1, 1 — the source fired no hook when it relinquished the conn, so the "+
			"engine that closes it owes the close and the hook",
			m.TransplantAdoptRefused, m.CloseCount, hooks.Load())
	}

	_, fd2 := adoptPair658(t)
	if err := s.e.AdoptConn(fd2, engine.Carryover{RemoteAddr: "127.0.0.1:659"}); err == nil {
		t.Errorf("AdoptConn after shutdown returned nil: the descriptor was queued on a " +
			"loop that will never run again, and the source believes it was handed off")
	} else {
		// On error the caller still owns fd2.
		if _, ferr := unix.FcntlInt(uintptr(fd2), unix.F_GETFD, 0); ferr != nil {
			t.Errorf("AdoptConn refused but closed the descriptor (%v); the caller owns it on error", ferr)
		}
		_ = unix.Close(fd2)
	}
}
