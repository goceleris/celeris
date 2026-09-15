//go:build linux

package iouring

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
	"github.com/goceleris/celeris/resource"
)

// celeris#658, the io_uring half. A standby worker with no listen socket and
// no connections parks on a Go channel that only ResumeAccept used to close.
// AdoptConn queues a driverActionAdopt, sets driverActionPending and writes
// the worker's eventfd — and returns nil, so the source has already let the
// connection go. A worker parked in a select is not waiting on its ring, so
// the adoption waited for the next ResumeAccept, forever if this engine stayed
// the standby, and Worker.shutdown never looked at the queue: the descriptor
// stayed open and owned by nobody.

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

// suspendedIOUring658 is an io_uring engine that has become a standby: accept
// paused and every worker parked in DRAINING→SUSPENDED.
type suspendedIOUring658 struct {
	e       *Engine
	workers []*Worker
	cancel  context.CancelFunc
	errCh   chan error
}

func startSuspendedIOUring658(t *testing.T, onDisconnect func(string)) *suspendedIOUring658 {
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
	}, transplantTestHandler{})
	if err != nil {
		t.Skipf("iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &suspendedIOUring658{e: e, cancel: cancel, errCh: make(chan error, 1)}
	go func() { s.errCh <- e.Listen(ctx) }()
	t.Cleanup(func() { s.stop(t) })

	for dl := time.Now().Add(10 * time.Second); time.Now().Before(dl); {
		if e.Addr() != nil && e.NumWorkers() > 0 {
			break
		}
		select {
		case lerr := <-s.errCh:
			s.errCh = nil
			t.Skipf("io_uring Listen failed here: %v", lerr)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e.Addr() == nil || e.NumWorkers() == 0 {
		t.Fatal("engine did not bind with workers")
	}
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	e.mu.Lock()
	s.workers = append([]*Worker(nil), e.workers...)
	e.mu.Unlock()
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); {
		if s.allSuspended() {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("workers never parked after PauseAccept (suspended=%v); the precondition "+
		"of this test is not reached", s.suspendedFlags())
	return nil
}

func (s *suspendedIOUring658) allSuspended() bool {
	for _, w := range s.workers {
		if !w.suspended.Load() {
			return false
		}
	}
	return len(s.workers) > 0
}

func (s *suspendedIOUring658) suspendedFlags() []bool {
	out := make([]bool, len(s.workers))
	for i, w := range s.workers {
		out[i] = w.suspended.Load()
	}
	return out
}

// stop cancels Listen and waits for it to return. Idempotent.
func (s *suspendedIOUring658) stop(t *testing.T) {
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

// TestAdoptConnWakesASuspendedWorker hands a live descriptor to an engine whose
// workers are ALREADY parked. AdoptConn returns nil, so the source has let go:
// the adoption has to complete, and the connection has to be served, without
// anyone calling ResumeAccept.
func TestAdoptConnWakesASuspendedWorker(t *testing.T) {
	s := startSuspendedIOUring658(t, nil)
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
		pending := make([]int32, len(s.workers))
		for i, w := range s.workers {
			pending[i] = w.driverActionPending.Load()
		}
		t.Fatalf("TransplantAdopted = %d 2s after AdoptConn returned nil, want 1: the "+
			"descriptor is queued (driverActionPending=%v) on a worker parked on its "+
			"wake channel (suspended=%v), and the eventfd AdoptConn wrote cannot wake "+
			"a worker that is not waiting on its ring — the connection is owned by no "+
			"engine (celeris#658)", got, pending, s.suspendedFlags())
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

// TestShutdownClosesAQueuedAdoption covers an adoption that is still queued
// when the worker shuts down. That is the state the lost wakeup leaves behind,
// and it stays reachable with the wakeup fixed — a worker woken by the kick
// checks its context before it drains the queue — so the queue is built here
// by hand, without the wakeup, to pin the shutdown path on its own. The
// descriptor must be closed (the peer sees EOF) and the close accounted like
// any other refused adoption, because the source fired no OnDisconnect.
//
// Then, once the worker is gone, AdoptConn must refuse: a nil return hands
// the descriptor to a queue nothing will ever drain, while an error lets the
// source take the connection back through its existing reclaim path.
func TestShutdownClosesAQueuedAdoption(t *testing.T) {
	var hooks atomic.Int64
	s := startSuspendedIOUring658(t, func(string) { hooks.Add(1) })
	client, fd := adoptPair658(t)

	w := s.workers[0]
	w.driverActionMu.Lock()
	w.driverActionQueue = append(w.driverActionQueue, driverAction{
		kind:       driverActionAdopt,
		adoptFD:    fd,
		adoptCarry: engine.Carryover{RemoteAddr: "127.0.0.1:658"},
	})
	w.driverActionPending.Store(1)
	w.driverActionMu.Unlock()

	s.stop(t)

	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := client.Read(make([]byte, 16))
	if !errors.Is(err, io.EOF) {
		t.Errorf("peer read = (%d, %v) after the engine shut down, want EOF: the "+
			"queued descriptor was never closed — Worker.shutdown does not look at "+
			"queued adoptions, so it stays open and owned by nobody (celeris#658)", n, err)
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
			"worker that will never run again, and the source believes it was handed off")
	} else {
		// On error the caller still owns fd2.
		if _, ferr := unix.FcntlInt(uintptr(fd2), unix.F_GETFD, 0); ferr != nil {
			t.Errorf("AdoptConn refused but closed the descriptor (%v); the caller owns it on error", ferr)
		}
		_ = unix.Close(fd2)
	}
}
