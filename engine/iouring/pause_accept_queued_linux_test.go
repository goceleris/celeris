//go:build linux

package iouring

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
	"github.com/goceleris/celeris/internal/deferlinger"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#662, the io_uring half. A pausing worker cancels its multishot
// accept, reaps the completions that are already in the CQ ring, and closes
// its listen socket. A connection whose handshake completed but which no
// accept completion had reached yet is still in the kernel accept queue at
// that close, and the kernel aborts it: the client, which may already have
// written its request, never gets a response, and the engine counts nothing.
//
// The rig is the epoll one: a handler that blocks until released holds every
// worker thread inside it, so no worker enters the ring and the next
// handshakes wait in the kernel queues with their requests written. The pause
// is set while no worker can observe it, then the handler is released.

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

func workers662(e *Engine) []*Worker {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*Worker(nil), e.workers...)
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
		t.Skipf("iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(h.gate) }) }
	var clients []net.Conn
	stopped := false
	t.Cleanup(func() {
		release()
		for _, c := range clients {
			_ = c.Close()
		}
		cancel()
		if stopped {
			return
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("engine did not stop within 10s")
		}
	})

	for dl := time.Now().Add(10 * time.Second); time.Now().Before(dl); {
		if e.Addr() != nil && e.NumWorkers() > 0 {
			break
		}
		select {
		case lerr := <-done:
			stopped = true
			t.Skipf("io_uring Listen failed here: %v", lerr)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e.Addr() == nil || e.NumWorkers() == 0 {
		t.Fatal("engine did not bind with workers")
	}
	// Block exactly the workers the engine runs: under a low RLIMIT_MEMLOCK
	// capWorkersToMemlock starts fewer than the two requested.
	ws := workers662(e)
	workers := len(ws)

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

	// Hold every worker inside the handler. A dial that lands on a worker
	// that is already held waits in its queue; it is counted among the
	// blockers and its outcome is not scored.
	blockers := 0
	for entered := 0; entered < workers; {
		if blockers == 64 {
			t.Fatalf("only %d of %d workers entered the handler after 64 dials", entered, workers)
		}
		dial()
		blockers++
		select {
		case <-h.entered:
			entered++
		case <-time.After(300 * time.Millisecond):
		}
	}

	const queued = 8
	queuedConns := make([]net.Conn, 0, queued)
	for range queued {
		queuedConns = append(queuedConns, dial())
	}
	time.Sleep(100 * time.Millisecond)
	before := e.Metrics()

	if pause {
		// No worker can observe the flag from inside the handler, so this
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
	// blocker that landed on a worker already held sits in that worker's queue
	// until the worker comes back, and nothing orders its accept before the
	// queued connections' responses: when every queued connection hashes to
	// another worker, all of them can answer first and AcceptCount reads one
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
		pause, workers, blockers, queued, tally, m.AcceptCount, before.AcceptCount, m.CloseCount, m.ErrorCount, connects.Load())

	if tally["200"] != queued {
		if pause {
			t.Errorf("PauseAccept dropped %d of %d connections that were waiting in the accept queue "+
				"with a request already sent (outcomes %v). A queued connection must be accepted "+
				"and served before the listen socket closes, not aborted by the close (celeris#662)",
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
			for _, w := range ws {
				if !w.listenFDClosed.Load() {
					return false
				}
			}
			return true
		}
		// The listeners close only after the pause's linger (celeris#662),
		// which starts once each worker is released from the handler.
		for dl := time.Now().Add(deferlinger.Linger() + 2*time.Second); !allClosed() && time.Now().Before(dl); {
			time.Sleep(5 * time.Millisecond)
		}
		if !allClosed() {
			t.Errorf("a worker never closed its listen socket after PauseAccept")
		}
		if c, derr := net.DialTimeout("tcp", addr, time.Second); derr == nil {
			_ = c.Close()
			t.Errorf("a dial after PauseAccept connected: the pause no longer stops accepting")
		}
	}

	for _, c := range clients {
		_ = c.Close()
	}
	allSuspended := func() bool {
		for _, w := range ws {
			if !w.suspended.Load() {
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
	d := bucketDeltas(before, m)
	t.Logf("after client close: active=%d accepts=%d closes=%d onDisconnect=%d errors=+%d buckets=%v",
		m.ActiveConnections, m.AcceptCount, m.CloseCount, disconnects.Load(), m.ErrorCount-before.ErrorCount, d)
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
	// The pause's own accept teardown, ECANCELED on each worker's in-flight
	// accept, is AcceptCancelled by design. Nothing else may move: the
	// AcceptOther this assertion caught was EINVAL from a stale re-arm aimed
	// at a closed listen descriptor whose number had been reused (celeris#662).
	for name, got := range d {
		if name != "AcceptCancelled" && got != 0 {
			t.Errorf("bucket %s moved by %d, want 0", name, got)
		}
	}
	if pause && !allSuspended() {
		t.Errorf("paused workers did not park after their connections closed: the SUSPENDED gate no longer engages")
	}
}

// TestPauseAcceptServesQueuedConnections pins celeris#662 on io_uring: every
// connection waiting in an accept queue when the pause lands is accepted,
// counted, served and closed normally, the listeners still close, and the
// paused workers still park once the connections are gone.
func TestPauseAcceptServesQueuedConnections(t *testing.T) { runPauseQueued662(t, true) }

// TestPauseAcceptQueuedControl is the same rig without the pause: it proves the
// rig delivers every queued request when nothing interferes.
func TestPauseAcceptQueuedControl(t *testing.T) { runPauseQueued662(t, false) }

// TestPauseAcceptDoesNotRearmTheListenSocketItCloses pins the re-arm half of
// celeris#662. The pause handled its cancelled accept's completion while
// w.listenFD still named the socket, so handleAccept queued a new accept for
// it, and the SQE reached the kernel after the close. On every pause that
// second accept failed with EBADF. Once the drain above made sibling workers
// allocate descriptors at the same moment, it failed with EINVAL on a sibling's
// connection in 9 of 50 runs of TestPauseAcceptServesQueuedConnections,
// because the closed number had already been reused.
//
// Each worker has one accept in flight, so a pause costs at most one failed
// accept per worker: its cancellation. The stale re-arm made it two per worker
// on every pause, so this bound catches it deterministically; the EINVAL
// needed a lost race.
func TestPauseAcceptDoesNotRearmTheListenSocketItCloses(t *testing.T) {
	eng, stop := startTestEngine(t)
	defer stop()

	addr := eng.Addr()
	if addr == nil {
		t.Skip("engine never bound")
	}
	c, err := net.DialTimeout("tcp", addr.String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.Close()

	workers := eng.NumWorkers()
	before := eng.Metrics()
	if err := eng.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	// A re-arm queued inside the pause completes on the worker's next
	// iteration, after PauseAccept has returned.
	time.Sleep(300 * time.Millisecond)
	after := eng.Metrics()

	d := bucketDeltas(before, after)
	total := after.ErrorCount - before.ErrorCount
	t.Logf("workers=%d pause cost: ErrorCount +%d, buckets %v", workers, total, d)
	for name, got := range d {
		if name != "AcceptCancelled" && got != 0 {
			t.Errorf("PauseAccept moved bucket %s by %d, want 0", name, got)
		}
	}
	if total > uint64(workers) {
		t.Errorf("PauseAccept cost %d failed accepts on %d workers, want at most one per worker "+
			"(its cancelled accept): the pause re-armed accept on the listen socket it was "+
			"closing, and that accept failed after the close (celeris#662)", total, workers)
	}
}
