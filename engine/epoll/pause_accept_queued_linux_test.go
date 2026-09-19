//go:build linux

package epoll

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#662. When a loop observed PauseAccept it accept4()ed every
// connection still waiting in the kernel accept queue and shut it down: a
// client that had completed its handshake and written a request before the
// pause read EOF instead of a response, and the engine counted nothing — no
// accept, no close, no error. Every adaptive switch pauses the old engine, so
// the switch itself dropped whatever the old engine's queues held; on a
// GitHub runner that was about a third of a 2048-connection ramp.
//
// The rig fills the queue deterministically instead of hoping load does: a
// handler that blocks until released holds every loop inside it, so the next
// handshakes complete in the kernel and wait there, each with its request
// already written. The pause is set while no loop can observe it, then the
// handler is released, so every loop reaches its pause branch with a full
// queue.

const queuedReq662 = "GET / HTTP/1.1\r\nHost: x\r\n\r\n"

type queuedBlockHandler662 struct {
	entered chan struct{}
	gate    chan struct{}
}

func (h *queuedBlockHandler662) HandleStream(_ context.Context, s *stream.Stream) error {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-h.gate
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}

// readOutcome662 reads one response and names how the exchange ended.
func readOutcome662(c net.Conn) string {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	switch {
	case err == nil:
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "status-" + strconv.Itoa(resp.StatusCode)
		}
		return "200"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "EOF"
	case errors.Is(err, syscall.ECONNRESET):
		return "RESET"
	case errors.Is(err, os.ErrDeadlineExceeded):
		return "TIMEOUT"
	default:
		return "other: " + err.Error()
	}
}

func loops662(e *Engine) []*Loop {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*Loop(nil), e.loops...)
}

func runPauseQueued662(t *testing.T, pause bool) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	var connects, disconnects atomic.Int64
	h := &queuedBlockHandler662{entered: make(chan struct{}, 64), gate: make(chan struct{})}
	e, err := New(resource.Config{
		Addr:         addr,
		Protocol:     engine.HTTP1,
		Resources:    resource.Resources{Workers: 2}, // the engine refuses fewer
		Logger:       slog.New(slog.DiscardHandler),
		OnConnect:    func(string) { connects.Add(1) },
		OnDisconnect: func(string) { disconnects.Add(1) },
	}, h)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(h.gate) }) }
	var clients []net.Conn
	t.Cleanup(func() {
		release()
		for _, c := range clients {
			_ = c.Close()
		}
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("engine did not stop within 10s")
		}
	})

	for dl := time.Now().Add(5 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(5 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine never bound")
	}
	// Block exactly the loops the engine runs.
	loops := loops662(e)
	workers := len(loops)

	dial := func() net.Conn {
		c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
		if derr != nil {
			t.Fatalf("dial: %v", derr)
		}
		clients = append(clients, c)
		if _, werr := c.Write([]byte(queuedReq662)); werr != nil {
			t.Fatalf("write request: %v", werr)
		}
		return c
	}

	// Hold every loop inside the handler. A dial that lands on a loop that is
	// already held just waits in that loop's queue, which is harmless: it is
	// counted among the blockers and its outcome is not scored.
	blockers := 0
	for entered := 0; entered < workers; {
		if blockers == 64 {
			t.Fatalf("only %d of %d loops entered the handler after 64 dials", entered, workers)
		}
		dial()
		blockers++
		select {
		case <-h.entered:
			entered++
		case <-time.After(300 * time.Millisecond):
		}
	}

	// Every loop is held: these handshakes complete in the kernel and sit in
	// the accept queues with their requests already sent.
	const queued = 8
	queuedConns := make([]net.Conn, 0, queued)
	for range queued {
		queuedConns = append(queuedConns, dial())
	}
	time.Sleep(100 * time.Millisecond)
	acceptsBeforeRelease := e.Metrics().AcceptCount

	if pause {
		// No loop can observe the flag from inside the handler, so this
		// returns on PauseAccept's own bounded wait with every listen socket
		// still open and every queue still full.
		if perr := e.PauseAccept(); perr != nil {
			t.Fatalf("PauseAccept: %v", perr)
		}
	}
	release()

	outcomes := make([]string, queued)
	var wg sync.WaitGroup
	for i, c := range queuedConns {
		wg.Go(func() { outcomes[i] = readOutcome662(c) })
	}
	// Wait for the blockers' responses too, though they are not scored. A
	// blocker that landed on a loop already held sits in that loop's queue
	// until the loop comes back, and nothing orders its accept before the
	// queued connections' responses: when every queued connection hashes to
	// another loop, all of them can answer first and AcceptCount reads one
	// short. That happened once on a GitHub runner (epoll: 10 of 11, and 11
	// after the clients closed). A response exists only after its connection
	// was accepted and counted, so once every client has one the count below
	// is exact.
	for _, c := range clients[:blockers] {
		wg.Go(func() { _ = readOutcome662(c) })
	}
	wg.Wait()
	tally := map[string]int{}
	for _, o := range outcomes {
		tally[o]++
	}

	m := e.Metrics()
	t.Logf("pause=%v workers=%d blockers=%d queued=%d outcomes=%v accepts=%d (before release %d) closes=%d errors=%d onConnect=%d",
		pause, workers, blockers, queued, tally, m.AcceptCount, acceptsBeforeRelease, m.CloseCount, m.ErrorCount, connects.Load())

	if tally["200"] != queued {
		if pause {
			t.Errorf("PauseAccept dropped %d of %d connections that were waiting in the accept queue "+
				"with a request already sent (outcomes %v). A queued connection must be accepted "+
				"through the normal registration path and served, not shut down (celeris#662)",
				queued-tally["200"], queued, tally)
		} else {
			t.Errorf("without a pause only %d of %d queued connections got a response (outcomes %v): "+
				"the rig itself is broken", tally["200"], queued, tally)
		}
	}
	if want := uint64(blockers + queued); m.AcceptCount != want {
		t.Errorf("AcceptCount = %d, want %d (%d blockers + %d queued): every connection the engine "+
			"takes off an accept queue must be counted", m.AcceptCount, want, blockers, queued)
	}
	if got := connects.Load(); got != int64(m.AcceptCount) {
		t.Errorf("OnConnect fired %d times for %d accepts", got, m.AcceptCount)
	}

	if pause {
		allClosed := func() bool {
			for _, l := range loops {
				if !l.listenFDClosed.Load() {
					return false
				}
			}
			return true
		}
		for dl := time.Now().Add(2 * time.Second); !allClosed() && time.Now().Before(dl); {
			time.Sleep(5 * time.Millisecond)
		}
		if !allClosed() {
			t.Errorf("a loop never closed its listen socket after PauseAccept")
		}
		if c, derr := net.DialTimeout("tcp", addr, time.Second); derr == nil {
			_ = c.Close()
			t.Errorf("a dial after PauseAccept connected: the pause no longer stops accepting")
		}
	}

	// Close every client. The served connections must close through the
	// normal path, and a paused engine must still park once it has none.
	for _, c := range clients {
		_ = c.Close()
	}
	allSuspended := func() bool {
		for _, l := range loops {
			if !l.suspended.Load() {
				return false
			}
		}
		return true
	}
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); {
		if e.Metrics().ActiveConnections == 0 && (!pause || allSuspended()) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	m = e.Metrics()
	t.Logf("after client close: active=%d accepts=%d closes=%d onDisconnect=%d errors=%d",
		m.ActiveConnections, m.AcceptCount, m.CloseCount, disconnects.Load(), m.ErrorCount)
	if m.ActiveConnections != 0 {
		t.Errorf("ActiveConnections = %d after every client closed, want 0", m.ActiveConnections)
	}
	if m.CloseCount != m.AcceptCount {
		t.Errorf("CloseCount = %d, AcceptCount = %d: every accepted connection must close through the engine",
			m.CloseCount, m.AcceptCount)
	}
	if got := disconnects.Load(); got != int64(m.CloseCount) {
		t.Errorf("OnDisconnect fired %d times for %d closes", got, m.CloseCount)
	}
	if m.ErrorCount != 0 {
		t.Errorf("ErrorCount = %d, want 0", m.ErrorCount)
	}
	if pause && !allSuspended() {
		t.Errorf("paused loops did not park after their connections closed: the SUSPENDED gate no longer engages")
	}
}

// TestPauseAcceptServesQueuedConnections pins celeris#662 on epoll: every
// connection waiting in an accept queue when the pause lands is accepted,
// counted, served and closed normally, the listeners still close, and the
// paused loops still park once the connections are gone.
func TestPauseAcceptServesQueuedConnections(t *testing.T) { runPauseQueued662(t, true) }

// TestPauseAcceptQueuedControl is the same rig without the pause. It proves the
// rig delivers every queued request when nothing interferes, so a failure of
// the paused arm is the pause and not the rig.
func TestPauseAcceptQueuedControl(t *testing.T) { runPauseQueued662(t, false) }
