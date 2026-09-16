//go:build linux

package adaptive

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
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// celeris#662 at the level where it actually bites: an engine switch.
//
// performSwitch resumes the new active engine and then pauses the old one.
// #663 taught that pause to serve what its accept queues hold, but both
// sub-engines set TCP_DEFER_ACCEPT=1 on their listen sockets, so a connection
// whose handshake completed while the old engine was active and which has sent
// no data is not in any accept queue: the kernel is holding it as a request
// socket. The pause closes the old engine's listen sockets, the request socket
// is orphaned, and the client's first request meets the new engine's LISTEN
// socket, which answers it with a reset. That is the shape the #660 CI job
// measured — a 2048-connection ramp losing about a third of its connections
// across a promotion, every sampled error a reset.
//
// The standby is built and bound BEFORE the measured phase. The lazy first
// build waits for the io_uring sub-engine to bind, which can take seconds, and
// the measured switch has to complete well inside the kernel's ~1 s deferral
// timer or the connections under test are rescued by that timer instead of by
// the engine — a pass that would mean nothing. The elapsed time is asserted for
// the same reason.

const (
	idleSwitchConns662       = 16
	idleSwitchBudget662      = 200 * time.Millisecond
	idleSwitchRescueBound662 = 900 * time.Millisecond
	idleSwitchReq662         = "GET / HTTP/1.1\r\nHost: x\r\n\r\n"
)

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

// serves662 reports whether a fresh dial to addr gets a 200, which is how the
// test waits for a resumed sub-engine to be back in the SO_REUSEPORT pool.
func serves662(addr string, within time.Duration) bool {
	for dl := time.Now().Add(within); time.Now().Before(dl); {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		_, werr := c.Write([]byte(idleSwitchReq662))
		ok := werr == nil && readOutcome662(c) == "200"
		_ = c.Close()
		if ok {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestSwitchKeepsHandshakedIdleConnections pins celeris#662 on the adaptive
// engine: connections established on the outgoing engine, which had not yet
// sent a request, must survive an epoll→io_uring promotion.
func TestSwitchKeepsHandshakedIdleConnections(t *testing.T) {
	// This test's claim IS the epoll→io_uring switch, so it needs io_uring.
	// The repo's guard for that is requireUpSwitch (celeris#641): it skips
	// where the switch cannot happen, and the CI `adaptive` job raises memlock
	// and sets CELERIS_REQUIRE_UPSWITCH=1, which turns the skip into a
	// failure so the job cannot go green by skipping. New() failing outright
	// is the same situation one step earlier, so it takes the same decision.
	e, err := New(resource.Config{
		Addr:     "127.0.0.1:0",
		Protocol: engine.HTTP1,
		Logger:   slog.New(slog.DiscardHandler),
	}, respHandler{}, nil)
	if err != nil {
		msg := "adaptive.New unsupported here: " + err.Error()
		if os.Getenv("CELERIS_REQUIRE_UPSWITCH") == "1" {
			t.Fatal(msg + " -- CELERIS_REQUIRE_UPSWITCH=1 forbids skipping")
		}
		t.Skip(msg)
	}
	requireUpSwitch(t, e)
	e.ctrl.cooldown = 0

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	var clients []net.Conn
	t.Cleanup(func() {
		for _, c := range clients {
			_ = c.Close()
		}
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("adaptive engine did not stop within 15s")
		}
	})
	for dl := time.Now().Add(5 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("adaptive engine never bound")
	}
	addr := e.Addr().String()

	if got := e.ActiveEngine().Type(); got != engine.Epoll {
		t.Fatalf("this test promotes epoll→io_uring and so must start on epoll, started on %v", got)
	}
	// Build and bind the lazy io_uring standby now, outside the measured
	// window, and come back to epoll. After this both sub-engines exist and
	// the measured switch is only a resume plus a pause.
	e.ForceSwitch()
	if got := e.ActiveEngine().Type(); got != engine.IOUring {
		t.Skipf("the io_uring standby did not come up here (active %v after a forced switch)", got)
	}
	e.ForceSwitch()
	if got := e.ActiveEngine().Type(); got != engine.Epoll {
		t.Fatalf("warm-up left %v active, want epoll", got)
	}
	if !serves662(addr, 5*time.Second) {
		t.Fatal("the epoll sub-engine never resumed serving after the warm-up switches")
	}

	base := e.Metrics()

	// Dial, and write NOTHING. Only epoll is listening: the io_uring standby
	// was paused by the second warm-up switch.
	tDial := time.Now()
	idle := make([]net.Conn, 0, idleSwitchConns662)
	for range idleSwitchConns662 {
		c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
		if derr != nil {
			t.Fatalf("dial: %v", derr)
		}
		clients = append(clients, c)
		idle = append(idle, c)
	}

	// Give an engine that CAN see these connections its chance to accept them.
	var before engine.EngineMetrics
	for dl := time.Now().Add(idleSwitchBudget662); ; {
		before = e.Metrics()
		if before.AcceptCount-base.AcceptCount >= uint64(idleSwitchConns662) || time.Now().After(dl) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	acceptedBeforeSwitch := before.AcceptCount - base.AcceptCount

	// The promotion. performSwitch pauses the outgoing epoll inline, so when
	// this returns its listen sockets are closed.
	e.ForceSwitch()
	elapsed := time.Since(tDial)
	if got := e.ActiveEngine().Type(); got != engine.IOUring {
		t.Fatalf("the measured switch left %v active, want io_uring", got)
	}
	// Vacuity guard, not a flake guard: past this bound the kernel's own ~1 s
	// SYN-ACK retransmit could have turned the deferred request sockets into
	// accepted connections, and a pass would say nothing about the engine.
	if elapsed >= idleSwitchRescueBound662 {
		t.Fatalf("the measured phase took %v, within reach of the kernel's ~1s SYN-ACK "+
			"retransmit: a deferred request socket would then have become a child through "+
			"that timer rather than through the engine, so this run cannot decide "+
			"celeris#662 either way", elapsed)
	}

	outcomes := make([]string, len(idle))
	var wg sync.WaitGroup
	for i, c := range idle {
		wg.Go(func() {
			if _, werr := c.Write([]byte(idleSwitchReq662)); werr != nil {
				outcomes[i] = "write: " + werr.Error()
				return
			}
			outcomes[i] = readOutcome662(c)
		})
	}
	wg.Wait()
	tally := map[string]int{}
	for _, o := range outcomes {
		tally[o]++
	}

	m := e.Metrics()
	t.Logf("idle=%d workers=%d outcomes=%v acceptsBeforeSwitch=%d accepts=%d switches=%d "+
		"errors=+%d elapsedToSwitch=%v memlockWorkerCeiling=%d",
		idleSwitchConns662, m.Workers, tally, acceptedBeforeSwitch,
		m.AcceptCount-base.AcceptCount, m.AdaptiveSwitches, m.ErrorCount-base.ErrorCount,
		elapsed, maxWorkersForMemlock())

	if acceptedBeforeSwitch != uint64(idleSwitchConns662) {
		t.Errorf("%d of %d handshake-complete connections had been accepted %v after they "+
			"connected, before the switch, want %d. TCP_DEFER_ACCEPT keeps a connection that "+
			"has sent no data out of the accept queue entirely, so the outgoing engine's "+
			"pause cannot see it and its listen-socket close resets it (celeris#662)",
			acceptedBeforeSwitch, idleSwitchConns662, idleSwitchBudget662, idleSwitchConns662)
	}
	if tally["200"] != idleSwitchConns662 {
		t.Errorf("the promotion dropped %d of %d connections that were established on the "+
			"outgoing engine before it paused (outcomes %v). A switch must not disconnect a "+
			"client that is already connected, whether or not it had sent its request yet "+
			"(celeris#662)", idleSwitchConns662-tally["200"], idleSwitchConns662, tally)
	}
}
