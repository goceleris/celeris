//go:build linux

package adaptive

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

// celeris#662 at the level where it bites: an engine switch.
//
// performSwitch resumes the incoming sub-engine and pauses the outgoing one.
// While a sub-engine's listeners have TCP_DEFER_ACCEPT on, which is the
// default, a client whose handshake completed on the outgoing engine but
// which has not sent its request yet is not in any accept queue: the kernel
// holds it and promotes it about one second after its SYN. A pause that closed
// the outgoing listeners at once orphaned it, and its first request met the
// incoming engine's listener, which answered with a reset. That is the shape
// the #660 CI job measured: a 2048-connection ramp losing about a third of
// its connections across a promotion.
//
// The outgoing engine now clears the option on its listeners and keeps them
// open for about 1.5 s before it closes them, and the switch does not wait for
// that. These tests check it end to end, in both directions, and check that
// the option itself is on again on every listener that serves.
//
// This file names no API that main lacks, so it can be copied onto main
// (where TestSwitchKeepsHandshakedIdleConnections must fail) or onto the
// round-3 gate (where TestAdaptiveListenersKeepDeferAcceptAcrossSwitches must
// fail). It waits for listener closes through the sockets themselves rather
// than through any hook.

const (
	idleSwitchConns662       = 16
	idleSwitchBudget662      = 200 * time.Millisecond
	idleSwitchRescueBound662 = 900 * time.Millisecond
	idleSwitchReq662         = "GET / HTTP/1.1\r\nHost: x\r\n\r\n"
	// idleSwitchCloseWait662 bounds every wait for an outgoing sub-engine's
	// listeners to close: the linger plus generous slack.
	idleSwitchCloseWait662 = 5 * time.Second
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

// adSock662 is one LISTEN socket of this process on the port under test.
type adSock662 struct {
	cookie  uint64
	deferOn bool
}

// listeners662 enumerates this process's LISTEN sockets bound to port via
// /proc/self/fd and reads SO_COOKIE and TCP_DEFER_ACCEPT on each. A
// getsockopt on a descriptor number touches no memory a sub-engine owns, and a
// number reused by a connection meanwhile is filtered out by SO_ACCEPTCONN and
// the port.
func listeners662(t *testing.T, port int) []adSock662 {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	var out []adSock662
	for _, ent := range ents {
		fd, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		link, err := os.Readlink("/proc/self/fd/" + ent.Name())
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		if v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN); err != nil || v != 1 {
			continue
		}
		sa, err := unix.Getsockname(fd)
		if err != nil {
			continue
		}
		p := -1
		switch s := sa.(type) {
		case *unix.SockaddrInet4:
			p = s.Port
		case *unix.SockaddrInet6:
			p = s.Port
		}
		if p != port {
			continue
		}
		c, _ := unix.GetsockoptUint64(fd, unix.SOL_SOCKET, unix.SO_COOKIE)
		d, _ := unix.GetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT)
		out = append(out, adSock662{cookie: c, deferOn: d != 0})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cookie < out[j].cookie })
	return out
}

func cookies662(ls []adSock662) map[uint64]bool {
	m := make(map[uint64]bool, len(ls))
	for _, s := range ls {
		m[s.cookie] = true
	}
	return m
}

// without662 returns the listeners whose cookie is not in exclude.
func without662(ls []adSock662, exclude map[uint64]bool) []adSock662 {
	var out []adSock662
	for _, s := range ls {
		if !exclude[s.cookie] {
			out = append(out, s)
		}
	}
	return out
}

func allOn662(ls []adSock662) bool {
	for _, s := range ls {
		if !s.deferOn {
			return false
		}
	}
	return len(ls) > 0
}

func describe662(ls []adSock662) string {
	var b strings.Builder
	for i, s := range ls {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%d:%v", s.cookie, s.deferOn)
	}
	return "[" + b.String() + "]"
}

// waitGone662 waits until no listener whose cookie is in set remains, and
// reports how long that took. It fails the test if the bound runs out.
func waitGone662(t *testing.T, port int, set map[uint64]bool, what string) time.Duration {
	t.Helper()
	start := time.Now()
	for dl := start.Add(idleSwitchCloseWait662); ; {
		gone := true
		for _, s := range listeners662(t, port) {
			if set[s.cookie] {
				gone = false
				break
			}
		}
		if gone {
			return time.Since(start)
		}
		if time.Now().After(dl) {
			t.Fatalf("%s: its listeners were still open %v later", what, idleSwitchCloseWait662)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// skipOrFailUpswitch662 skips, or fails when CELERIS_REQUIRE_UPSWITCH=1 (the
// CI adaptive job) forbids skipping.
func skipOrFailUpswitch662(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("CELERIS_REQUIRE_UPSWITCH") == "1" {
		t.Fatal(msg + " -- CELERIS_REQUIRE_UPSWITCH=1 forbids skipping")
	}
	t.Skip(msg)
}

// requireMigrateReqZero662: with net.ipv4.tcp_migrate_req on, the kernel
// moves the requests of a closing listener to another listener in the
// SO_REUSEPORT group, the incoming engine's, and a rescue would be the
// kernel's rather than the engine's.
func requireMigrateReqZero662(t *testing.T) {
	t.Helper()
	b, err := os.ReadFile("/proc/sys/net/ipv4/tcp_migrate_req")
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if v := strings.TrimSpace(string(b)); err != nil || v != "0" {
		skipOrFailUpswitch662(t, "net.ipv4.tcp_migrate_req reads %q (%v): the kernel would "+
			"migrate deferred requests itself", v, err)
	}
}

// startAdaptive662 builds and starts an adaptive engine on cfg (Addr
// 127.0.0.1:0 when empty), with the controller frozen so that only the test
// switches it, and returns it with its port. It must start on epoll.
func startAdaptive662(t *testing.T, cfg resource.Config) (*Engine, int) {
	t.Helper()
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}
	if cfg.Protocol == 0 {
		cfg.Protocol = engine.HTTP1
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	e, err := New(cfg, respHandler{}, nil)
	if err != nil {
		skipOrFailUpswitch662(t, "adaptive.New unsupported here: %v", err)
	}
	// Only the test switches this engine. ForceSwitch bypasses the freeze.
	e.FreezeSwitching()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() {
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
	if got := e.ActiveEngine().Type(); got != engine.Epoll {
		t.Fatalf("these tests switch between the two sub-engines starting from epoll, started on %v", got)
	}
	return e, e.Addr().(*net.TCPAddr).Port
}

// buildStandby662 builds and binds the lazy io_uring standby with a switch,
// and returns the epoll listeners from before it and the io_uring listeners
// it added. io_uring is active on return.
func buildStandby662(t *testing.T, e *Engine, port int) (epollSet, iouringSet map[uint64]bool) {
	t.Helper()
	epollSet = cookies662(listeners662(t, port))
	e.ForceSwitch()
	if got := e.ActiveEngine().Type(); got != engine.IOUring {
		skipOrFailUpswitch662(t, "the io_uring standby did not come up here (active %v after "+
			"a forced switch)", got)
	}
	iouringSet = cookies662(without662(listeners662(t, port), epollSet))
	if len(iouringSet) == 0 {
		t.Fatal("the io_uring standby is active but no new listener appeared on the port")
	}
	return epollSet, iouringSet
}

func workers662(e *Engine) (epoll, iouring int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.primary != nil {
		epoll = e.primary.Metrics().Workers
	}
	if e.secondary != nil {
		iouring = e.secondary.Metrics().Workers
	}
	return epoll, iouring
}

// runIdleSwitch662 is the rig of TestSwitchKeepsHandshakedIdleConnections and
// its revert twin. Clients complete their handshakes on the OUTGOING engine
// and send nothing; the switch starts while the kernel still holds them; they
// stay silent until every listener the outgoing engine had has closed, and
// only then write.
func runIdleSwitch662(t *testing.T, promote bool) {
	requireMigrateReqZero662(t)
	e, port := startAdaptive662(t, resource.Config{})
	addr := e.Addr().String()

	// Build the io_uring standby outside the measured window. Then wait out
	// the linger of whichever engine the warm-up left, so exactly one
	// engine's listeners are on the port when the measurement starts.
	epollSet, iouringSet := buildStandby662(t, e, port)
	var outgoing, incoming engine.EngineType
	if promote {
		e.ForceSwitch()
		if got := e.ActiveEngine().Type(); got != engine.Epoll {
			t.Fatalf("warm-up left %v active, want epoll", got)
		}
		waitGone662(t, port, iouringSet, "the io_uring standby after the warm-up")
		outgoing, incoming = engine.Epoll, engine.IOUring
	} else {
		waitGone662(t, port, epollSet, "the epoll standby after the warm-up")
		outgoing, incoming = engine.IOUring, engine.Epoll
	}
	if !serves662(addr, 5*time.Second) {
		t.Fatalf("the %v sub-engine is not serving after the warm-up", outgoing)
	}

	before := listeners662(t, port)
	if !allOn662(before) {
		t.Fatalf("premise: every listener must have TCP_DEFER_ACCEPT on before the switch, "+
			"read %s", describe662(before))
	}
	base := e.Metrics()

	// Dial, and write NOTHING. Only the outgoing engine is listening.
	tDial := time.Now()
	idle := make([]net.Conn, 0, idleSwitchConns662)
	t.Cleanup(func() {
		for _, c := range idle {
			_ = c.Close()
		}
	})
	for range idleSwitchConns662 {
		c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
		if derr != nil {
			t.Fatalf("dial: %v", derr)
		}
		idle = append(idle, c)
	}
	time.Sleep(idleSwitchBudget662)
	hidden := e.Metrics().AcceptCount - base.AcceptCount
	if hidden != 0 {
		t.Fatalf("premise: %d of %d silent clients were accepted within %v, want 0: "+
			"TCP_DEFER_ACCEPT is not hiding them, so the switch below tests nothing",
			hidden, idleSwitchConns662, idleSwitchBudget662)
	}

	tSwitch := time.Now()
	if d := tSwitch.Sub(tDial); d >= idleSwitchRescueBound662 {
		t.Fatalf("undecidable: %v from the dials to the switch is within reach of the kernel's "+
			"one-second SYN-ACK retransmit", d)
	}
	e.ForceSwitch()
	switchWall := time.Since(tSwitch)
	if got := e.ActiveEngine().Type(); got != incoming {
		t.Fatalf("the measured switch left %v active, want %v", got, incoming)
	}
	closedAfter := waitGone662(t, port, cookies662(before), "the outgoing sub-engine")
	acceptedBeforeWrite := e.Metrics().AcceptCount - base.AcceptCount

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
	ew, iw := workers662(e)
	m := e.Metrics()
	t.Logf("%v->%v idle=%d workers epoll=%d io_uring=%d listenersBefore=%s hidden200ms=%d "+
		"dialToSwitch=%v switchWall=%v outgoingClosedAfter=%v acceptedBeforeWrite=%d outcomes=%v "+
		"switches=%d errors=+%d memlockWorkerCeiling=%d",
		outgoing, incoming, idleSwitchConns662, ew, iw, describe662(before), hidden,
		tSwitch.Sub(tDial), switchWall, closedAfter, acceptedBeforeWrite, tally,
		m.AdaptiveSwitches, m.ErrorCount-base.ErrorCount, maxWorkersForMemlock())

	if tally["200"] != idleSwitchConns662 {
		t.Errorf("the %v->%v switch dropped %d of %d clients that had completed their handshake "+
			"on the outgoing engine before it paused (outcomes %v). A switch must not disconnect "+
			"a client that is already connected, whether or not it had sent its request yet "+
			"(celeris#662)", outgoing, incoming, idleSwitchConns662-tally["200"],
			idleSwitchConns662, tally)
	}
}

// TestSwitchKeepsHandshakedIdleConnections pins celeris#662 on the adaptive
// engine: clients established on the outgoing epoll engine, which had not yet
// sent a request, survive an epoll->io_uring promotion. It fails on main, where
// the outgoing engine's listeners close at once.
func TestSwitchKeepsHandshakedIdleConnections(t *testing.T) {
	runIdleSwitch662(t, true)
}

// TestSwitchKeepsHandshakedIdleConnectionsOnRevert is the same in the other
// direction: clients established on the outgoing io_uring engine survive an
// io_uring->epoll revert, the direction the always-on error-rate revert takes.
func TestSwitchKeepsHandshakedIdleConnectionsOnRevert(t *testing.T) {
	runIdleSwitch662(t, false)
}

// TestAdaptiveListenersKeepDeferAcceptAcrossSwitches pins what the default
// engine's listeners look like in the steady state, which is what
// celeris#662's fix must leave alone: TCP_DEFER_ACCEPT is on on every listener
// before any switch, on every incoming engine's listener after one, on the
// same listeners again after a flap back within the outgoing engine's linger
// (the resume restores it), and on every listener once each linger has ended.
// Only an outgoing engine's listeners may read it off, and only until they
// close. The round-3 gate turned the option off on every listener of a
// switch-capable engine, and this test fails there.
func TestAdaptiveListenersKeepDeferAcceptAcrossSwitches(t *testing.T) {
	e, port := startAdaptive662(t, resource.Config{})

	before := listeners662(t, port)
	if !allOn662(before) {
		t.Fatalf("before any switch every listener must have TCP_DEFER_ACCEPT on, read %s",
			describe662(before))
	}
	epollSet, iouringSet := buildStandby662(t, e, port)
	incoming := without662(listeners662(t, port), epollSet)
	if !allOn662(incoming) {
		t.Errorf("after the promotion the incoming io_uring listeners read %s, want all on",
			describe662(incoming))
	}

	// Flap back at once, inside epoll's linger: epoll's listeners are resumed.
	e.ForceSwitch()
	if got := e.ActiveEngine().Type(); got != engine.Epoll {
		t.Fatalf("the flap left %v active, want epoll", got)
	}
	var epollNow []adSock662
	for dl := time.Now().Add(time.Second); ; {
		epollNow = without662(listeners662(t, port), iouringSet)
		if allOn662(epollNow) || time.Now().After(dl) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !allOn662(epollNow) {
		t.Errorf("after a flap back within the linger the epoll listeners read %s, want all on: "+
			"the resume did not restore the option", describe662(epollNow))
	}
	iouringClosed := waitGone662(t, port, iouringSet, "the io_uring engine after the flap")
	settled := listeners662(t, port)
	t.Logf("before=%s incomingAfterPromotion=%s epollAfterFlapWithinLinger=%s "+
		"iouringClosedAfter=%v settled=%s", describe662(before), describe662(incoming),
		describe662(epollNow), iouringClosed, describe662(settled))
	if !allOn662(settled) {
		t.Errorf("after the io_uring linger ended the listeners read %s, want all on", describe662(settled))
	}

	// A full cycle with each linger run out: every listener re-created by a
	// resume must have the option too.
	for i, want := range []engine.EngineType{engine.IOUring, engine.Epoll} {
		out := cookies662(listeners662(t, port))
		e.ForceSwitch()
		if got := e.ActiveEngine().Type(); got != want {
			t.Fatalf("switch %d left %v active, want %v", i, got, want)
		}
		var in []adSock662
		// A resumed engine re-creates its listeners on its own thread; one
		// that was not parked sees the resume at its next wait.
		for dl := time.Now().Add(3 * time.Second); ; {
			in = without662(listeners662(t, port), out)
			if allOn662(in) || time.Now().After(dl) {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		if !allOn662(in) {
			t.Errorf("switch %d to %v: the incoming listeners read %s, want all on",
				i, want, describe662(in))
		}
		waitGone662(t, port, out, fmt.Sprintf("the outgoing engine of switch %d", i))
		if ls := listeners662(t, port); !allOn662(ls) {
			t.Errorf("after switch %d's linger ended the listeners read %s, want all on", i, describe662(ls))
		}
	}
}
