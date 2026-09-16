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

// celeris#662, the half of the loss class that #663's pause drain cannot
// reach.
//
// createListenSocket sets TCP_DEFER_ACCEPT=1 on a listen socket unless the
// config turns it off. While it is set, a connection whose handshake has
// completed but which has sent no data is held by the kernel as a
// TCP_NEW_SYN_RECV request socket and never enters the accept queue at all:
// accept4 answers EAGAIN for it. #663's drain accepts what the accept queue
// holds, so it cannot see such a connection, and when the listen socket closes
// the orphaned request socket is reset — the client's first request gets no
// response, and the engine counts no accept, no close and no error.
//
// The fix is resource.Config.DisableDeferAccept, which the rig below sets on
// its fix arm: the connections then land in the accept queue like any other
// and #663's drain serves them. The option stays ON by default, because an
// engine that never pauses would pay +14-21% ns/op on connection churn to
// give it up -- the adaptive engine sets it only for sub-engines it can
// actually switch between, and a standalone engine's owner must ask
// (celeris#675). So what the fix arm exercises is an engine configured the
// way one that intends to pause must be, and the deferred arm exercises the
// default. TestListenSocketDeferAcceptFollowsConfig pins both directions at
// the socket.
//
// The rig needs no blocking handler, unlike the queued-connection rig in
// pause_accept_queued_linux_test.go: here the kernel itself is what hides the
// connections, so an idle engine is the sharpest arrangement there is. It dials
// N connections, writes NOTHING, and reads the accept count BEFORE the pause —
// with the option on that count is 0, which is the whole defect — then pauses,
// and only then sends the requests.
//
// The 1 s deferral timer is this rig's only clock, and it is a vacuity hazard
// rather than a flake hazard. At num_timeout 1 the kernel retransmits the
// SYN-ACK and the ACK it elicits DOES create the child, so a rig that let more
// than about a second pass between the dials and the listen-socket close would
// find its connections rescued by that timer and would pass on main for the
// wrong reason. The measured phase is bounded two orders of magnitude below
// that bound, and the elapsed time is asserted, so an overrun fails the test
// loudly instead of hiding inside it.

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
//	true  -- the fix: the engine asks for no TCP_DEFER_ACCEPT, so a
//	         handshake-complete connection that has sent nothing reaches the
//	         accept queue and the pause's drain serves it.
//	false -- the residual (celeris#675): the option is on, the kernel holds
//	         those connections out of the accept queue, and no drain can
//	         reach them.
//
// Both arms ship, and they are each other's control: the fix arm asserts the
// connections ARE accepted before the pause, the deferred arm asserts they
// are NOT. A DisableDeferAccept that stopped reaching createListenSocket, or
// that inverted, fails one of them.
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
		// The RESIDUAL arm (celeris#675). TCP_DEFER_ACCEPT is on, so the
		// kernel holds every one of these connections OUT of the accept queue
		// and no drain can reach them. That mechanism is deterministic, so it
		// is what this arm asserts.
		//
		// What the client finally sees is NOT asserted: after the listen
		// socket closes the orphaned request socket is reset here, but on a
		// slow enough run the kernel's ~1s SYN-ACK retransmit could create
		// the child instead. The outcomes are logged above; only the
		// mechanism is pinned.
		if acceptedBeforePause != 0 {
			t.Errorf("%d of %d connections were accepted within %v while TCP_DEFER_ACCEPT "+
				"was set, want 0. The kernel must hold a handshake-complete connection "+
				"that has sent no data out of the accept queue entirely -- that is the "+
				"premise celeris#662's fix rests on, and if it no longer holds the fix "+
				"needs re-deriving rather than re-running",
				acceptedBeforePause, idleConns662, idlePrePauseBudget662)
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

// TestPauseAcceptKeepsHandshakedIdleConnections pins the residual half of
// celeris#662 on epoll: a connection whose handshake completed before the pause
// must be served even though it had not sent a byte when the pause landed.
func TestPauseAcceptKeepsHandshakedIdleConnections(t *testing.T) {
	runPauseIdle662(t, true, true)
}

// TestPauseAcceptDeferredIdleConnectionsAreHidden is the same rig with
// TCP_DEFER_ACCEPT left ON -- the default, and what a standalone engine still
// gets (celeris#675). It asserts the MECHANISM the fix rests on: the kernel
// holds a handshake-complete connection that has sent no data out of the
// accept queue, so the pause's drain provably cannot reach it.
//
// It is also this file's failing arm. The two arms make opposite assertions
// about the same counter, so a DisableDeferAccept that stopped reaching
// createListenSocket, or that inverted, fails one of them.
func TestPauseAcceptDeferredIdleConnectionsAreHidden(t *testing.T) {
	runPauseIdle662(t, true, false)
}

// TestPauseAcceptIdleControl is the fix arm's rig without the pause: same
// config, same write-nothing clients, and every connection is served. It is
// what makes the paused arm's failure attributable to the PAUSE rather than
// to the rig's unusual clients.
//
// It is NOT a "passes on main" control, and this file cannot offer one: the
// rig names resource.Config.DisableDeferAccept, a field main does not have,
// so none of these tests compiles against main. The artifact that fails on
// unmodified main is adaptive's TestSwitchKeepsHandshakedIdleConnections,
// which names no new API and can be checked out onto main verbatim.
func TestPauseAcceptIdleControl(t *testing.T) { runPauseIdle662(t, false, true) }

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
// It calls createListenSocket directly rather than reading a Loop's listenFD.
// That field is owned by the loop goroutine, so reading it from the test
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
