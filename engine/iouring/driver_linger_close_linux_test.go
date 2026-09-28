//go:build linux

package iouring

import (
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#735: since celeris#691 the engine holds its own duplicate of every
// driver socket (driverConn.opFD) and closes it when it finalizes the conn.
// After UnregisterConn and the caller's own close that duplicate is the
// socket's last reference, so its close(2) does the socket's release, and
// with SO_LINGER set and data unsent to a peer that is not reading, the
// release waits up to the linger time for the FIN's ACK. The engine closed it
// on the LockOSThread'd worker goroutine, so every other connection of the
// ring waited as long: no CQE processed, no accept, no flush.
//
// The rig is the issue's probe: a victim driver conn V on the same worker as
// a lingering socket L; L is unregistered and closed, and 100 ms later V's
// peer writes a byte. Measured on the unfixed engine: 2.9 s from the write to
// V's onRecv with a 3 s linger, against tens of microseconds without one.

// lingerStallBudget is the most V's byte may take to reach onRecv. A worker
// that is not blocked delivers it in microseconds; a blocked one holds it for
// the rest of the linger (lingerStallSecs less the 100 ms the test waits
// first). The gap absorbs a loaded -race run.
const (
	lingerStallSecs   = 3
	lingerStallBudget = time.Second
)

// socketIdentity is what a descriptor names: its device and inode.
type socketIdentity struct{ dev, ino uint64 }

func identityOf(t *testing.T, fd int) socketIdentity {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatalf("fstat(%d): %v", fd, err)
	}
	return socketIdentity{st.Dev, st.Ino}
}

// stillNames reports whether fd is open and still names the file id.
func stillNames(fd int, id socketIdentity) bool {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return false
	}
	return st.Dev == id.dev && st.Ino == id.ino
}

// lingerBacklog is how much the test leaves unsent behind a lingering close.
// celeris#691's rig (lingeringTCPSocket, which this replaces) shrank the send
// buffer to 4 KiB, so only a few KiB waited behind its FIN, and a receiver
// whose buffer is full can still take them in: it collapses its queue when a
// zero-window probe comes, opens the window, the FIN is ACKed and the close
// returns. On a GitHub x86 runner that ended one 3 s linger in 1 of 5 runs
// within 100 ms (PR #744, job 108681501232). A backlog of megabytes behind a
// 4 KiB receive buffer cannot be absorbed that way, so the close lingers
// until the peer drains. The send buffer asked for is capped at twice
// net.core.wmem_max (416 KiB at the default), and the rig refuses a backlog
// under 128 KiB.
const lingerBacklog = 4 << 20

// deeplyLingeringTCPSocket is celeris#691's rig with lingerBacklog left
// unsent: a connected, non-blocking TCP socket whose peer never reads, with
// SO_LINGER {1, linger}, so its last close waits the whole linger time.
// drain reads the peer, and the close then returns.
func deeplyLingeringTCPSocket(t *testing.T, linger int) (fd int, drain func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	peer, err := ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	_ = peer.(*net.TCPConn).SetReadBuffer(4096)
	f, err := c.(*net.TCPConn).File() // a blocking duplicate; the net.Conn is done with
	_ = c.Close()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	fd, err = unix.Dup(int(f.Fd()))
	_ = f.Close()
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatalf("set non-blocking: %v", err)
	}
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, lingerBacklog)
	chunk := make([]byte, 64<<10)
	queued := 0
	for queued < lingerBacklog {
		n, err := unix.Write(fd, chunk)
		if errors.Is(err, unix.EAGAIN) {
			break
		}
		if err != nil {
			t.Fatalf("fill the send path: %v", err)
		}
		queued += n
	}
	if queued < 128<<10 {
		t.Fatalf("apparatus: only %d bytes could be queued behind the close, want a backlog of at least 128 KiB", queued)
	}
	if err := unix.SetsockoptLinger(fd, unix.SOL_SOCKET, unix.SO_LINGER, &unix.Linger{Onoff: 1, Linger: int32(linger)}); err != nil {
		t.Fatalf("SO_LINGER: %v", err)
	}
	t.Logf("celeris735 LINGER rig backlog_bytes=%d linger=%ds", queued, linger)
	drain = func() {
		go func() { _, _ = io.Copy(io.Discard, peer) }()
	}
	return fd, drain
}

// lingeringDriver registers a lingering socket on w, settles its RECV, and
// returns it with its onClose channel, the engine's duplicate of it and what
// that duplicate names.
func lingeringDriver(t *testing.T, w *Worker, linger int) (fd int, drain func(), closed chan error, opFD int, id socketIdentity) {
	t.Helper()
	fd, drain = deeplyLingeringTCPSocket(t, linger)
	if linger == 0 {
		// The control: SO_LINGER off, so the close returns at once.
		if err := unix.SetsockoptLinger(fd, unix.SOL_SOCKET, unix.SO_LINGER, &unix.Linger{Onoff: 0}); err != nil {
			t.Fatalf("clear SO_LINGER: %v", err)
		}
	}
	id = identityOf(t, fd)
	closed = make(chan error, 1)
	if err := w.RegisterConn(fd, func([]byte) {}, func(err error) { closed <- err }); err != nil {
		t.Fatalf("RegisterConn(lingering): %v", err)
	}
	settleDriverRecv(t, w, fd)
	dc := driverConnOf(w, fd)
	if dc == nil {
		t.Fatal("the lingering conn is not registered")
	}
	return fd, drain, closed, dc.opFD, id
}

// unregisterAndCloseInOrder does what every in-tree driver does, UnregisterConn
// and then its own close of fd, with the worker parked in between (p), so the
// worker finalizes the conn only after the caller's close. The engine's
// duplicate is then the socket's last reference, and the engine's close does
// the lingering release. Unparked, the worker can win that race: its close
// then drops a reference, returns at once, and the caller's close lingers on
// the caller's goroutine instead (1 in 4 runs of the m8 shape, and on CI).
func unregisterAndCloseInOrder(t *testing.T, p *workerPark, wl interface{ UnregisterConn(int) error }, fd int) {
	t.Helper()
	p.park()
	if err := wl.UnregisterConn(fd); err != nil {
		p.release()
		t.Fatalf("UnregisterConn: %v", err)
	}
	if err := unix.Close(fd); err != nil {
		p.release()
		t.Fatalf("close the caller's fd: %v", err)
	}
	p.release()
}

// waitFinalized waits until the worker has finalized fd's conn: it has left
// the driver map, so the close of the engine's duplicate has begun.
func waitFinalized(t *testing.T, w *Worker, fd int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); driverConnOf(w, fd) != nil; {
		if time.Now().After(deadline) {
			t.Fatalf("fd %d was never finalized after UnregisterConn", fd)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitCloseStarted waits until the engine's close(2) of its duplicate has
// begun: close(2) takes the number out of the descriptor table before the
// socket's release, so opFD no longer names the socket once it has started.
func waitCloseStarted(t *testing.T, opFD int, id socketIdentity) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); stillNames(opFD, id); {
		if time.Now().After(deadline) {
			t.Fatalf("the engine's duplicate (fd %d) of the socket was never closed", opFD)
		}
		time.Sleep(time.Millisecond)
	}
}

// closeInLingerWait reports whether the last close of the socket id names is
// still in its linger wait, and the socket's TCP state (hex, as
// /proc/net/tcp prints it) when it is.
//
// /proc/net/tcp lists the IPv4 TCP sockets of the network namespace, each
// with the inode of the socket file that owns it. tcp_close orphans the
// socket (sock_orphan, after which that inode reads 0) only once its linger
// wait has ended, however it ends: the FIN acknowledged, the linger time up,
// or a signal. So once the last close has begun, a row that still carries
// the socket's inode says that close has not returned.
func closeInLingerWait(t *testing.T, id socketIdentity) (waiting bool, state string) {
	t.Helper()
	b, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		t.Fatalf("apparatus: read /proc/net/tcp: %v", err)
	}
	ino := strconv.FormatUint(id.ino, 10)
	for i, line := range strings.Split(string(b), "\n") {
		// sl local rem st tx:rx tr:when retrnsmt uid timeout inode ...
		if f := strings.Fields(line); i > 0 && len(f) > 9 && f[9] == ino {
			return true, f[3]
		}
	}
	return false, ""
}

// lingerAttempts is how many sockets the linger arm tries for one whose
// close lingers before it fails as apparatus.
const lingerAttempts = 5

// TestDriverLingeringCloseDoesNotStallTheWorker is the issue's measurement as
// a test. The linger arm fails on the unfixed engine; the no-linger arm is
// the control that shows the rig measures a worker that is not blocked.
//
// A close that should linger does not always do so. The kernel leaves the
// linger wait early on a pending signal (the review of #744 measured that in
// this package's other celeris#735 test), and then the close returns at
// once, the fixed engine fires onClose at once, and the attempt looks like
// the no-linger control: V's byte in microseconds and onClose before it
// (3 of 30 linger runs on main's CI, celeris#763 item 5). Such an attempt
// proves nothing about either check, so each attempt reads whether the close
// is still in its linger wait (closeInLingerWait) after V was served and after
// onClose was looked for: an attempt whose close has returned by then is
// void, and the arm tries again on a new engine and socket. It fails as
// apparatus when no attempt's close lingers. A V held past the budget fails
// at once, lingering or not.
func TestDriverLingeringCloseDoesNotStallTheWorker(t *testing.T) {
	t.Run("no_linger_control", func(t *testing.T) {
		// With SO_LINGER off the close never waits, so the probe must read
		// it as returned: a probe that reads it lingering is broken.
		if lingeringCloseAttempt(t, "no_linger_control", 0, 1) {
			t.Fatal("apparatus: /proc/net/tcp still shows the socket owned by a file after a close with " +
				"SO_LINGER off: closeInLingerWait cannot tell a returned close from a lingering one")
		}
	})
	t.Run("linger", func(t *testing.T) {
		void := 0
		for attempt := 1; attempt <= lingerAttempts; attempt++ {
			lingered := lingeringCloseAttempt(t, "linger", lingerStallSecs, attempt)
			switch {
			case t.Failed():
				t.Logf("celeris735 LINGER arm=linger attempts=%d void=%d result=fail", attempt, void)
				return
			case lingered:
				t.Logf("celeris735 LINGER arm=linger attempts=%d void=%d result=lingered", attempt, void)
				return
			}
			void++
		}
		t.Fatalf("apparatus: L's close did not linger in any of %d attempts (%d void), so neither "+
			"check was made", lingerAttempts, void)
	})
}

// lingeringCloseAttempt is one attempt of
// TestDriverLingeringCloseDoesNotStallTheWorker on a new engine: L with
// SO_LINGER {1, linger}, or SO_LINGER off when linger is 0. It reports
// whether L's close was still in its linger wait once V had been served and
// onClose looked for; only then are the two checks evidence. V's latency
// fails at once either way: nothing but a blocked worker holds V's byte for
// a second.
func lingeringCloseAttempt(t *testing.T, arm string, linger, attempt int) (lingered bool) {
	t.Helper()
	e, stop := startTestEngine(t)
	defer stop()
	w, wl := e.workers[0], e.WorkerLoop(0)
	v := registerSettledDriver(t, w, wl)
	defer v.unregisterAndWait(t, wl)
	p := newWorkerPark(t, w, wl)
	defer p.d.unregisterAndWait(t, wl)

	fd, drain, closed, opFD, id := lingeringDriver(t, w, linger)
	drained := false
	defer func() {
		if !drained {
			drain()
		}
	}()
	unregisterAndCloseInOrder(t, p, wl, fd)
	waitFinalized(t, w, fd)
	finalized := time.Now()
	waitCloseStarted(t, opFD, id)
	// The issue's probe: give the worker the cancel and the start of the
	// close before V's peer writes.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	if _, err := unix.Write(v.peer, []byte{'v'}); err != nil {
		t.Fatalf("write V's peer: %v", err)
	}
	var lat time.Duration
	select {
	case <-v.recv:
		lat = time.Since(start)
	case <-time.After(10 * time.Second):
		lat = -1
	}
	var closedEarly bool
	select {
	case err := <-closed:
		closed <- err // put it back for the check below
		closedEarly = true
	default:
	}
	// After onClose was looked for: a close still waiting now was waiting
	// when V was served, and when onClose was looked for.
	lingered, state := closeInLingerWait(t, id)
	stalled := lat < 0 || lat > lingerStallBudget
	early := linger > 0 && lingered && closedEarly
	verdict := "void" // the close did not linger: neither check is evidence
	switch {
	case stalled || early:
		verdict = "fail"
	case linger == 0:
		verdict = "control"
	case lingered:
		verdict = "valid"
	}
	t.Logf("celeris735 LINGER arm=%s attempt=%d linger=%ds v_onrecv_ms=%.3f onclose_before_v=%v close_lingering=%v tcp_state=%q verdict=%s",
		arm, attempt, linger, float64(lat)/1e6, closedEarly, lingered, state, verdict)
	if stalled {
		t.Errorf("celeris#735: V's byte reached onRecv after %v (budget %v) while another conn's "+
			"close lingered (SO_LINGER %d s): the worker was blocked in close(2)", lat, lingerStallBudget, linger)
	}
	if early {
		t.Errorf("L's onClose fired while its close still lingered: a driver must see the " +
			"socket closed before onClose (engine/provider.go)")
	}

	// The peer drains, the close returns, and then onClose fires,
	// once, with the unregister's nil error, the socket closed.
	drain()
	drained = true
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("onClose(%v), want nil after UnregisterConn", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("L's onClose never fired")
	}
	t.Logf("celeris735 LINGER arm=%s attempt=%d onclose_after_finalize_ms=%.1f", arm, attempt, float64(time.Since(finalized))/1e6)
	if stillNames(opFD, id) {
		t.Errorf("onClose fired with the engine's duplicate (fd %d) still open on the socket", opFD)
	}
	return lingered
}

// TestDriverShutdownWaitsForAHandedOffClose: a conn finalized before the
// engine stops has its close handed off the worker; the engine stops while
// that close lingers. Shutdown must wait for it and still fire the conn's
// onClose, with the conn's own nil error, not errEngineShutdown and not
// never: the loop that would have fired it has stopped (celeris#735).
func TestDriverShutdownWaitsForAHandedOffClose(t *testing.T) {
	e, stop := startTestEngine(t)
	t.Cleanup(stop) // idempotent; the test calls it itself below
	w, wl := e.workers[0], e.WorkerLoop(0)
	p := newWorkerPark(t, w, wl)
	fd, drain, closed, opFD, id := lingeringDriver(t, w, 2)
	unregisterAndCloseInOrder(t, p, wl, fd)
	waitFinalized(t, w, fd)

	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	// Let the stop reach the worker while the close still lingers, then let
	// the close finish.
	time.Sleep(200 * time.Millisecond)
	drain()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("the engine did not stop")
	}
	select {
	case err := <-closed:
		if errors.Is(err, errEngineShutdown) || err != nil {
			t.Errorf("onClose(%v), want nil: the conn was unregistered before the shutdown", err)
		}
	default:
		t.Error("the engine stopped without firing the onClose of a conn whose close it had handed off")
	}
	if stillNames(opFD, id) {
		t.Errorf("the engine stopped with its duplicate (fd %d) of the socket still open", opFD)
	}
}
