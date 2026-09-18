//go:build linux

package epoll

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

// celeris#662: what TCP_DEFER_ACCEPT hides from a pause, and the two
// configurations whose pause needs no linger.
//
// createListenSocket sets TCP_DEFER_ACCEPT=1 on a listen socket unless the
// config turns it off. While it is set, a connection whose handshake has
// completed but which has sent no data is held by the kernel as a
// TCP_NEW_SYN_RECV request socket and does not enter the accept queue: accept4
// answers EAGAIN for it, so neither the loop nor #663's pause drain can see
// it. Closing the listener then resets it. The pause therefore clears the
// option and lingers before it closes the listener; that is exercised in
// pause_accept_linger_linux_test.go.
//
// This file keeps the pieces that do not linger:
//
//   - TestPauseAcceptDeferredIdleConnectionsAreHidden is the PREMISE control,
//     with no pause at all: with the option on, silent clients are not
//     accepted, and the kernel's TCPDeferAcceptDrop counter says why. If that
//     ever stops holding, the linger is solving a problem that is not there
//     and needs re-deriving.
//   - TestPauseAcceptKeepsHandshakedIdleConnections is the DisableDeferAccept
//     arm, the instant lossless pause: with the option off the silent
//     clients are in the accept queue, the pause drains and serves them, and
//     it closes at once.
//   - TestPauseAcceptIdleControl is that arm without the pause.
//   - TestListenSocketDeferAcceptFollowsConfig pins the option on the socket.
//
// The rig needs no blocking handler, unlike the queued-connection rig in
// pause_accept_queued_linux_test.go: the kernel itself is what hides these
// connections, so an idle engine is the sharpest arrangement there is. It
// dials N connections, writes NOTHING, reads the accept count, pauses (or
// not), and only then sends the requests.
//
// The kernel's one-second SYN-ACK timer is this rig's only clock, and it is a
// vacuity hazard rather than a flake hazard: past about a second the kernel
// promotes a deferred connection itself. The measured phase of the paused arm
// is bounded well below that, and the elapsed time is asserted, so an overrun
// fails loudly instead of hiding.

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
// blocks a loop: the connections under test are invisible to the engine, not
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

// runPauseIdle662 drives the rig. disableDefer selects the ARM:
//
//	true  -- DisableDeferAccept: a handshake-complete connection that has sent
//	         nothing reaches the accept queue, the engine accepts it at once,
//	         and a pause drains, serves and closes without lingering.
//	false -- the default: the option is on and the kernel holds those
//	         connections out of the accept queue. Only the premise control
//	         uses this arm here, and it does not pause.
//
// The two arms make opposite assertions about the same counter, so a
// DisableDeferAccept that stopped reaching createListenSocket, or that
// inverted, fails one of them.
func runPauseIdle662(t *testing.T, pause, disableDefer bool) {
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
		// The fix under test (celeris#662), and the arm selector. With it
		// clear the kernel keeps the idle connections out of the accept queue,
		// and the pause drain -- which can only see that queue -- never gets
		// the chance to serve them.
		DisableDeferAccept: disableDefer,
		Logger:             slog.New(slog.DiscardHandler),
		OnConnect:          func(string) { connects.Add(1) },
		OnDisconnect:       func(string) { disconnects.Add(1) },
	}, idleRespHandler662{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
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
	loops := loops662(e)
	workers := len(loops)

	dropsBefore := tcpDeferAcceptDrops662()
	dropsBeforeOK, _ := tcpDeferAcceptDropsL662()
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
	dropsNow, dropsOK := tcpDeferAcceptDropsL662()
	dropsDeltaOK := dropsNow - dropsBeforeOK

	var elapsedToClose time.Duration
	if pause {
		if perr := e.PauseAccept(); perr != nil {
			t.Fatalf("PauseAccept: %v", perr)
		}
		allClosed := func() bool {
			for _, l := range loops {
				if !l.listenFDClosed.Load() {
					return false
				}
			}
			return true
		}
		for dl := time.Now().Add(2 * time.Second); !allClosed() && time.Now().Before(dl); {
			time.Sleep(time.Millisecond)
		}
		if !allClosed() {
			t.Fatal("a loop never closed its listen socket after PauseAccept")
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
		"errors=%d onConnect=%d TCPDeferAcceptDrop delta=%d elapsedToListenClose=%v",
		pause, workers, idleConns662, tally, acceptedBeforePause,
		m.AcceptCount-base.AcceptCount, m.CloseCount-base.CloseCount,
		m.ErrorCount-base.ErrorCount, connects.Load(), dropsDelta, elapsedToClose)

	if !disableDefer {
		// The PREMISE arm (no pause). TCP_DEFER_ACCEPT is on, so the kernel
		// holds every one of these connections OUT of the accept queue until
		// its data arrives. That is deterministic, and it is the premise of
		// celeris#662's linger.
		if pause {
			t.Fatal("the rig's option-on arm is the pause-free premise control; the paused " +
				"option-on case is pause_accept_linger_linux_test.go")
		}
		if acceptedBeforePause != 0 {
			t.Errorf("%d of %d connections were accepted within %v while TCP_DEFER_ACCEPT "+
				"was set, want 0. The kernel must hold a handshake-complete connection "+
				"that has sent no data out of the accept queue -- that is the premise "+
				"celeris#662's linger rests on, and if it no longer holds the fix "+
				"needs re-deriving rather than re-running",
				acceptedBeforePause, idleConns662, idlePrePauseBudget662)
		}
		if dropsOK && dropsDeltaOK < uint64(idleConns662) {
			t.Errorf("TCPDeferAcceptDrop moved by %d over %d silent dials, want >= %d: the "+
				"kernel did not report deferring them", dropsDeltaOK, idleConns662, idleConns662)
		}
		if tally["200"] != idleConns662 {
			t.Errorf("without a pause only %d of %d deferred connections got a response once "+
				"they wrote (outcomes %v): the rig itself is broken", tally["200"], idleConns662, tally)
		}
		if want := base.AcceptCount + uint64(idleConns662); m.AcceptCount != want {
			t.Errorf("AcceptCount = %d, want %d: every connection the engine serves "+
				"must be counted", m.AcceptCount, want)
		}
	} else {
		if pause && acceptedBeforePause != uint64(idleConns662) {
			t.Errorf("%d of %d handshake-complete connections had been accepted %v after "+
				"they connected, before any pause, want %d. This engine sets "+
				"DisableDeferAccept, so these connections must reach the accept queue; "+
				"while TCP_DEFER_ACCEPT is on the kernel keeps them out of it entirely, "+
				"so the drain #663 added cannot see them and the listen-socket close "+
				"resets them (celeris#662)",
				acceptedBeforePause, idleConns662, idlePrePauseBudget662, idleConns662)
		}
		if tally["200"] != idleConns662 {
			if pause {
				t.Errorf("PauseAccept dropped %d of %d connections whose handshake had "+
					"completed before the pause (outcomes %v). A client that is already "+
					"connected must be served, whether or not it had sent its request "+
					"yet (celeris#662)", idleConns662-tally["200"], idleConns662, tally)
			} else {
				t.Errorf("without a pause only %d of %d idle connections got a response "+
					"(outcomes %v): the rig itself is broken",
					tally["200"], idleConns662, tally)
			}
		}
		if want := base.AcceptCount + uint64(idleConns662); m.AcceptCount != want {
			t.Errorf("AcceptCount = %d, want %d: every connection the engine serves "+
				"must be counted", m.AcceptCount, want)
		}
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
		t.Errorf("CloseCount = %d, AcceptCount = %d: every accepted connection must close "+
			"through the engine", m.CloseCount, m.AcceptCount)
	}
	if got := disconnects.Load(); got != int64(m.CloseCount) {
		t.Errorf("OnDisconnect fired %d times for %d closes", got, m.CloseCount)
	}
	if m.ErrorCount != 0 {
		t.Errorf("ErrorCount = %d, want 0", m.ErrorCount)
	}
	if pause && !allSuspended() {
		t.Errorf("paused loops did not park after their connections closed: the SUSPENDED gate " +
			"no longer engages")
	}
}

// TestPauseAcceptKeepsHandshakedIdleConnections is the DisableDeferAccept
// arm on epoll: the instant, lossless pause. With the option off, a connection
// whose handshake completed before the pause is in the accept queue even
// though it has not sent a byte, so the pause drains and serves it and
// closes the listeners at once, with no linger.
func TestPauseAcceptKeepsHandshakedIdleConnections(t *testing.T) {
	runPauseIdle662(t, true, true)
}

// TestPauseAcceptDeferredIdleConnectionsAreHidden is the pause-free PREMISE
// control of celeris#662's linger: with TCP_DEFER_ACCEPT on (the default),
// silent clients whose handshakes completed are not accepted, and the kernel
// counts each deferral in TCPDeferAcceptDrop. Once they write, they are
// served.
//
// It is also this file's opposite arm. It and the DisableDeferAccept arm make
// opposite assertions about the same counter, so a DisableDeferAccept that
// stopped reaching createListenSocket, or that inverted, fails one of them.
func TestPauseAcceptDeferredIdleConnectionsAreHidden(t *testing.T) {
	runPauseIdle662(t, false, false)
}

// TestPauseAcceptIdleControl is the DisableDeferAccept arm's rig without the
// pause: same config, same write-nothing clients, and every connection is
// served. It is what makes the paused arm's result attributable to the pause
// rather than to the rig's unusual clients.
func TestPauseAcceptIdleControl(t *testing.T) { runPauseIdle662(t, false, true) }

// TestListenSocketDeferAcceptFollowsConfig pins both directions of the option
// on the socket itself, with no engine involved.
//
// deferAccept=false is DisableDeferAccept: with TCP_DEFER_ACCEPT clear a
// handshake-complete connection that has sent no data enters the accept
// queue, where the pause's drain reaches it, so the pause needs no linger.
//
// deferAccept=true is what every engine that does not ask otherwise gets, and
// it is asserted here too rather than left implicit: a silent flip of the
// default would change what every accept costs, with no other test to catch
// it.
//
// It calls createListenSocket directly rather than reading a listenFD field.
// That field is owned by the event-loop goroutine, so reading it from the
// test goroutine would be a data race, and -race would flag the test rather
// than the defect.
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
