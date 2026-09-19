//go:build linux

package iouring

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// celeris#662, the io_uring half of the loss class that #663's pause drain
// cannot reach.
//
// createListenSocket sets TCP_DEFER_ACCEPT=1 on a listen socket unless the
// config turns it off. While it is set, a connection whose handshake has
// completed but which has sent no data is held by the kernel as a
// TCP_NEW_SYN_RECV request socket and never enters the accept queue: neither a
// multishot accept completion nor the pause's acceptQueuedOnPause sweep can
// produce it. When the pausing worker closes its listen socket the orphaned
// request socket is reset, and the client's first request gets no response
// while the engine counts no accept, no close and no error.
//
// The fix is resource.Config.DisableDeferAccept, which the rig below sets: the
// connections then land in the accept queue like any other and #663's drain
// serves them. The option stays ON by default, because an engine that never
// pauses would pay +14-21% ns/op on connection churn to give it up, so what
// this rig exercises is an engine configured the way one that intends to pause
// must be. TestListenSocketDeferAcceptFollowsConfig pins both directions.
//
// The rig needs no blocking handler, unlike the queued-connection rig in
// pause_accept_queued_linux_test.go: the kernel itself is what hides these
// connections, so an idle engine is the sharpest arrangement there is. It dials
// N connections, writes NOTHING, and reads the accept count BEFORE the pause —
// on main that count is 0, which is the whole defect — then pauses, and only
// then sends the requests.
//
// The 1 s deferral timer is this rig's only clock, and it is a vacuity hazard
// rather than a flake hazard: past about a second the kernel's SYN-ACK
// retransmit creates the children itself and main would pass for the wrong
// reason. The measured phase is bounded two orders of magnitude below that,
// and the elapsed time is asserted, so an overrun fails loudly instead of
// hiding.
//
// workers= is logged on every run. Under a low RLIMIT_MEMLOCK
// capWorkersToMemlock starts fewer workers than requested (one at the CI's
// 8 MiB), and no cross-engine claim may be read off a run without it.

const (
	// idleConns662 matches the issue's measured rig (RESET 8 of 8).
	idleConns662 = 8
	// idlePrePauseBudget662 is how long the rig waits for the engine to
	// accept connections that have sent nothing. An engine that can see them
	// takes microseconds; main never accepts them at all and burns the whole
	// budget, which is why the budget sits far below the 1 s deferral timer.
	idlePrePauseBudget662 = 200 * time.Millisecond
	// idleDeferRescueBound662 is the elapsed time from the first dial to the
	// listen-socket close beyond which the kernel's SYN-ACK retransmit could
	// have created the children itself, making the run undecidable.
	idleDeferRescueBound662 = 900 * time.Millisecond
)

// idleRespHandler662 answers every request immediately. Nothing in this rig
// blocks a worker: the connections under test are invisible to the engine, not
// merely delayed by it.
type idleRespHandler662 struct{}

func (idleRespHandler662) HandleStream(_ context.Context, s *stream.Stream) error {
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}

// tcpDeferAcceptDrops662 reads the netns-wide TcpExtTCPDeferAcceptDrop counter,
// which the kernel bumps each time it drops a bare handshake ACK because
// TCP_DEFER_ACCEPT is set on the listener. It is the mechanism's own witness:
// its delta over the dials is at least idleConns662 while the defect is
// present and exactly 0 once the option is gone. Logged, never asserted — the
// counter is per network namespace, so anything else in the namespace moves it
// too.
func tcpDeferAcceptDrops662() uint64 {
	b, err := os.ReadFile("/proc/net/netstat")
	if err != nil {
		return 0
	}
	lines := strings.Split(string(b), "\n")
	for i := 0; i+1 < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "TcpExt:") || !strings.HasPrefix(lines[i+1], "TcpExt:") {
			continue
		}
		names := strings.Fields(lines[i])
		vals := strings.Fields(lines[i+1])
		if len(names) != len(vals) {
			continue
		}
		for j, n := range names {
			if n == "TCPDeferAcceptDrop" {
				v, perr := strconv.ParseUint(vals[j], 10, 64)
				if perr != nil {
					return 0
				}
				return v
			}
		}
	}
	return 0
}

func runPauseIdle662(t *testing.T, pause bool) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	var connects, disconnects atomic.Int64
	e, err := New(resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2}, // the engine refuses fewer
		// The fix under test (celeris#662). Without it the kernel keeps the
		// idle connections out of the accept queue and the pause drain, which
		// can only see that queue, never gets the chance to serve them.
		DisableDeferAccept: true,
		Logger:             slog.New(slog.DiscardHandler),
		OnConnect:          func(string) { connects.Add(1) },
		OnDisconnect:       func(string) { disconnects.Add(1) },
	}, idleRespHandler662{})
	if err != nil {
		t.Skipf("iouring engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()

	var clients []net.Conn
	stopped := false
	t.Cleanup(func() {
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
	// Under a low RLIMIT_MEMLOCK capWorkersToMemlock starts fewer than the two
	// requested; the count is logged with every result below.
	ws := workers662(e)
	workers := len(ws)

	dropsBefore := tcpDeferAcceptDrops662()
	base := e.Metrics()

	// Dial, and write NOTHING. Each handshake completes in the kernel. With
	// TCP_DEFER_ACCEPT set the connection would stay a request socket and
	// never reach the accept queue; with DisableDeferAccept it enters the
	// queue, so an engine that is working accepts it having read no bytes.
	tDial := time.Now()
	idle := make([]net.Conn, 0, idleConns662)
	for range idleConns662 {
		c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
		if derr != nil {
			t.Fatalf("dial: %v", derr)
		}
		clients = append(clients, c)
		idle = append(idle, c)
	}

	// Give an engine that CAN see these connections its chance to accept them.
	var before engine.EngineMetrics
	for dl := time.Now().Add(idlePrePauseBudget662); ; {
		before = e.Metrics()
		if before.AcceptCount-base.AcceptCount >= uint64(idleConns662) || time.Now().After(dl) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	acceptedBeforePause := before.AcceptCount - base.AcceptCount
	dropsDelta := tcpDeferAcceptDrops662() - dropsBefore

	var elapsedToClose time.Duration
	if pause {
		if perr := e.PauseAccept(); perr != nil {
			t.Fatalf("PauseAccept: %v", perr)
		}
		allClosed := func() bool {
			for _, w := range ws {
				if !w.listenFDClosed.Load() {
					return false
				}
			}
			return true
		}
		for dl := time.Now().Add(2 * time.Second); !allClosed() && time.Now().Before(dl); {
			time.Sleep(time.Millisecond)
		}
		if !allClosed() {
			t.Fatal("a worker never closed its listen socket after PauseAccept")
		}
		elapsedToClose = time.Since(tDial)
		// Vacuity guard, not a flake guard: past this bound the kernel's own
		// SYN-ACK retransmit could have turned the deferred request sockets
		// into accepted connections, and a pass would say nothing about the
		// engine.
		if elapsedToClose >= idleDeferRescueBound662 {
			t.Fatalf("the measured phase took %v, within reach of the kernel's ~1s SYN-ACK "+
				"retransmit: a deferred request socket would then have become a child through "+
				"that timer rather than through the engine, so this run cannot decide "+
				"celeris#662 either way", elapsedToClose)
		}
	}

	// Only now do the clients send their requests.
	outcomes := make([]string, len(idle))
	var wg sync.WaitGroup
	for i, c := range idle {
		wg.Go(func() {
			if _, werr := c.Write([]byte(queuedReq662)); werr != nil {
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
	t.Logf("pause=%v workers=%d idle=%d outcomes=%v acceptsBeforePause=%d accepts=%d closes=%d "+
		"errors=+%d onConnect=%d TCPDeferAcceptDrop delta=%d elapsedToListenClose=%v",
		pause, workers, idleConns662, tally, acceptedBeforePause,
		m.AcceptCount-base.AcceptCount, m.CloseCount-base.CloseCount,
		m.ErrorCount-base.ErrorCount, connects.Load(), dropsDelta, elapsedToClose)

	if pause && acceptedBeforePause != uint64(idleConns662) {
		t.Errorf("%d of %d handshake-complete connections had been accepted %v after they "+
			"connected, before any pause, want %d. This engine sets DisableDeferAccept, so "+
			"these connections must reach the accept queue; while TCP_DEFER_ACCEPT is on "+
			"the kernel keeps them out of it entirely, so the drain #663 added cannot see "+
			"them and the listen-socket close resets them (celeris#662)",
			acceptedBeforePause, idleConns662, idlePrePauseBudget662, idleConns662)
	}
	if tally["200"] != idleConns662 {
		if pause {
			t.Errorf("PauseAccept dropped %d of %d connections whose handshake had completed "+
				"before the pause (outcomes %v). A client that is already connected must be "+
				"served, whether or not it had sent its request yet (celeris#662)",
				idleConns662-tally["200"], idleConns662, tally)
		} else {
			t.Errorf("without a pause only %d of %d idle connections got a response "+
				"(outcomes %v): the rig itself is broken", tally["200"], idleConns662, tally)
		}
	}
	if want := base.AcceptCount + uint64(idleConns662); m.AcceptCount != want {
		t.Errorf("AcceptCount = %d, want %d: every connection the engine serves must be counted",
			m.AcceptCount, want)
	}
	if got := connects.Load(); got != int64(m.AcceptCount) {
		t.Errorf("OnConnect fired %d times for %d accepts", got, m.AcceptCount)
	}

	if pause {
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
	d := bucketDeltas(base, m)
	t.Logf("after client close: active=%d accepts=%d closes=%d onDisconnect=%d errors=+%d buckets=%v",
		m.ActiveConnections, m.AcceptCount, m.CloseCount, disconnects.Load(),
		m.ErrorCount-base.ErrorCount, d)
	if m.ActiveConnections != 0 {
		t.Errorf("ActiveConnections = %d after every client closed, want 0", m.ActiveConnections)
	}
	if m.CloseCount != m.AcceptCount {
		t.Errorf("CloseCount = %d, AcceptCount = %d: every accepted connection must close "+
			"through the engine", m.CloseCount, m.AcceptCount)
	}
	if got := disconnects.Load(); got != int64(m.CloseCount) {
		t.Errorf("OnDisconnect fired %d times for %d closes", got, m.CloseCount)
	}
	// The pause's own accept teardown — ECANCELED on each worker's in-flight
	// accept — is AcceptCancelled by design. Nothing else may move.
	for name, got := range d {
		if name != "AcceptCancelled" && got != 0 {
			t.Errorf("bucket %s moved by %d, want 0", name, got)
		}
	}
	if pause && !allSuspended() {
		t.Errorf("paused workers did not park after their connections closed: the SUSPENDED " +
			"gate no longer engages")
	}
}

// TestPauseAcceptKeepsHandshakedIdleConnections pins the residual half of
// celeris#662 on io_uring: a connection whose handshake completed before the
// pause must be served even though it had not sent a byte when the pause
// landed.
func TestPauseAcceptKeepsHandshakedIdleConnections(t *testing.T) { runPauseIdle662(t, true) }

// TestPauseAcceptIdleControl is the same rig without the pause. It passes on
// main — the deferred request sockets become children as soon as their data
// arrives, and the engine accepts and serves them — which is what makes the
// paused arm's failure attributable to the pause rather than to the rig's
// unusual write-nothing clients.
func TestPauseAcceptIdleControl(t *testing.T) { runPauseIdle662(t, false) }

// TestListenSocketDeferAcceptFollowsConfig pins both directions of the
// celeris#662 fix on the socket itself, with no engine involved.
//
// deferAccept=false is the fix. With TCP_DEFER_ACCEPT clear a
// handshake-complete connection that has sent no data enters the accept queue,
// which is the only place the pause's drain can reach it before the listen
// socket closes.
//
// deferAccept=true is what every engine that does not ask otherwise still
// gets, and it is asserted here too rather than left implicit. The default was
// kept deliberately: dropping the option costs +14-21% ns/op and +2.4-3.5 us
// of server CPU per connection on churn (measured, 30 rounds per arm), so a
// silent flip of it would be a throughput regression with no other test to
// catch it.
//
// It calls createListenSocket directly rather than reading a Worker's listenFD.
// That field is owned by the worker goroutine, so reading it from the test
// goroutine would be a data race, and -race would flag the test rather than the
// defect.
func TestListenSocketDeferAcceptFollowsConfig(t *testing.T) {
	for _, tc := range []struct {
		name        string
		deferAccept bool
		wantSet     bool
	}{
		{"disabled is the celeris#662 fix", false, false},
		{"enabled is the throughput default", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd, err := createListenSocket("127.0.0.1:0", tc.deferAccept)
			if err != nil {
				t.Fatalf("createListenSocket: %v", err)
			}
			t.Cleanup(func() { _ = unix.Close(fd) })

			// The kernel reports the deferral as a timeout in seconds, not as
			// the literal 1 that was set, so read it as a flag.
			v, err := unix.GetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT)
			if err != nil {
				t.Fatalf("getsockopt TCP_DEFER_ACCEPT: %v", err)
			}
			if gotSet := v != 0; gotSet != tc.wantSet {
				t.Errorf("createListenSocket(deferAccept=%v): TCP_DEFER_ACCEPT reads %d "+
					"(set=%v), want set=%v. Set, the kernel holds a handshake-complete "+
					"connection out of the accept queue until data arrives, so the pause "+
					"drain never sees it and the listen-socket close resets it "+
					"(celeris#662); clear, the engine pays an extra wakeup per idle "+
					"connection, which is why it is not the default",
					tc.deferAccept, v, gotSet, tc.wantSet)
			}
		})
	}
}
