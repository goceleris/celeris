//go:build linux

package iouring

// A transplant's carried request can detach during the replay (a WebSocket or
// SSE handler on a pipelined request). The middleware goroutine then writes
// through the guarded writeFn, which reads and writes cs.writeBuf, cs.sendBuf
// and cs.sending under cs.detachMu, while the worker's replay flush
// (attachAdoptedFD in sync mode, replayCarriedAsync on an AsyncHandlers
// worker) used to call flushSend on the same fields with no lock. handleRecv's
// twin (respondAndArm) and epoll's flushCarriedReplay take the lock.
//
// These tests are data-race tests: they fail only under -race (go test -race),
// with a DATA RACE report at the replay's flushSend against the guarded
// writeFn. Each carry test has a control that sends the same request through
// the recv path, which has always been clean.

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// carryDetachHandler detaches on GET /detach and starts a goroutine that
// writes frames through the detached conn's raw writer.
type carryDetachHandler struct{ done chan struct{} }

func (h *carryDetachHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	var path string
	for _, hd := range s.GetHeaders() {
		if hd[0] == ":path" {
			path = hd[1]
		}
	}
	if path != "/detach" || s.OnDetach == nil || s.OnWSRawWrite == nil {
		return nil
	}
	s.OnDetach()
	raw := s.OnWSRawWrite()
	started := make(chan struct{})
	frame := bytes.Repeat([]byte{'f'}, 64)
	go func() {
		close(started)
		for range 4000 {
			raw(frame)
		}
		close(h.done)
	}()
	<-started
	return nil
}

// carryDetachAsyncHandler is the same handler on an AsyncHandlers worker where
// /detach is a sync route (it is served inline by the replay).
type carryDetachAsyncHandler struct{ carryDetachHandler }

func (h *carryDetachAsyncHandler) RouteAsync(_, path string) bool { return path == "/never" }
func (h *carryDetachAsyncHandler) HasAsyncRoutes() bool           { return true }

func carryDetachDrain(c net.Conn, d time.Duration) {
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, _ = io.Copy(io.Discard, c)
}

func waitCarryDetachWriter(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the detached writer never finished")
	}
	// Let the worker's flush and the last guarded writes settle so a race
	// between them is reported inside this test.
	time.Sleep(200 * time.Millisecond)
}

func runCarryDetach(t *testing.T, async bool, h stream.Handler, done <-chan struct{}) {
	t.Helper()
	e := start543(t, async, h)
	client, fd := adoptable543(t)
	go carryDetachDrain(client, 3*time.Second)
	if err := e.AdoptConn(fd, engine.Carryover{RemoteAddr: client.LocalAddr().String(), Buffered: []byte("GET /detach HTTP/1.1\r\nHost: x\r\n\r\n")}); err != nil {
		t.Fatalf("AdoptConn: %v", err)
	}
	waitCarryDetachWriter(t, done)
}

func runRecvDetach(t *testing.T, async bool, h stream.Handler, done <-chan struct{}) {
	t.Helper()
	e := start543(t, async, h)
	client, err := net.Dial("tcp", e.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if _, err := client.Write([]byte("GET /detach HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	go carryDetachDrain(client, 3*time.Second)
	waitCarryDetachWriter(t, done)
}

func TestCarriedReplayFlushHoldsDetachMuSync(t *testing.T) {
	h := &carryDetachHandler{done: make(chan struct{})}
	runCarryDetach(t, false, h, h.done)
}

func TestCarriedReplayFlushHoldsDetachMuAsync(t *testing.T) {
	h := &carryDetachAsyncHandler{carryDetachHandler{done: make(chan struct{})}}
	runCarryDetach(t, true, h, h.done)
}

func TestRecvDetachFlushControlSync(t *testing.T) {
	h := &carryDetachHandler{done: make(chan struct{})}
	runRecvDetach(t, false, h, h.done)
}

func TestRecvDetachFlushControlAsync(t *testing.T) {
	h := &carryDetachAsyncHandler{carryDetachHandler{done: make(chan struct{})}}
	runRecvDetach(t, true, h, h.done)
}
