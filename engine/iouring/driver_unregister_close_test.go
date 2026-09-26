//go:build linux

package iouring

// celeris#691. UnregisterConn only queues the unregister; the worker issues
// the ASYNC_CANCEL later, and IORING_ASYNC_CANCEL_FD resolves its descriptor
// number when the worker issues it. A caller that closes fd right after
// UnregisterConn, as the memcached, redis and postgres drivers all do, made
// that cancel miss: the RECV armed on the socket kept its own reference and
// stayed armed, onClose never fired, and the peer never saw EOF. A reused
// number made the cancel land on whatever socket then held it.
//
// Most tests here park the worker goroutine inside another driver conn's
// onRecv (callbacks run on the worker goroutine, engine/provider.go) while
// the caller acts, so the worker issues the cancel strictly after the close.

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
)

// errSendFailed stands in for the error a failed driver SEND reports.
var errSendFailed = errors.New("celeris#691 test: SEND failed")

func driverConnOf(w *Worker, fd int) *driverConn {
	w.driverMu.RLock()
	defer w.driverMu.RUnlock()
	return w.driverConns[fd]
}

// settleDriverRecv waits until fd's RECV is armed, then 50 ms more, so the
// worker's next submit has handed it to the kernel.
func settleDriverRecv(t *testing.T, w *Worker, fd int) {
	t.Helper()
	dc := driverConnOf(w, fd)
	if dc == nil {
		t.Fatalf("fd %d is not registered", fd)
	}
	for deadline := time.Now().Add(2 * time.Second); ; {
		dc.mu.Lock()
		armed := dc.recvArmed
		dc.mu.Unlock()
		if armed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fd %d: the RECV was never armed", fd)
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
}

// testDriver is one driver conn registered on a worker, and its peer.
type testDriver struct {
	fd, peer   int
	peerClosed bool
	closed     chan error    // onClose's argument
	closeErr   error         // what expectReleased received from onClose
	recv       chan struct{} // one per onRecv call
}

func newTestDriver(t *testing.T, wl engine.WorkerLoop) *testDriver {
	t.Helper()
	d := &testDriver{closed: make(chan error, 1), recv: make(chan struct{}, 16)}
	d.fd, d.peer = nonblockSocketPair(t)
	if err := wl.RegisterConn(d.fd, func([]byte) {
		select {
		case d.recv <- struct{}{}:
		default:
		}
	}, func(err error) { d.closed <- err }); err != nil {
		t.Fatalf("RegisterConn(%d): %v", d.fd, err)
	}
	return d
}

func registerSettledDriver(t *testing.T, w *Worker, wl engine.WorkerLoop) *testDriver {
	t.Helper()
	d := newTestDriver(t, wl)
	settleDriverRecv(t, w, d.fd)
	return d
}

// waitClosed waits up to 2 s for onClose, and returns its argument.
func (d *testDriver) waitClosed() (fired bool, err error) {
	select {
	case err = <-d.closed:
		return true, err
	case <-time.After(2 * time.Second):
		return false, nil
	}
}

// peerSees reads d's peer end for up to 2 s: "EOF" once the socket is
// closed, "open" if something still holds it.
func (d *testDriver) peerSees() string {
	var one [1]byte
	for deadline := time.Now().Add(2 * time.Second); ; {
		n, err := unix.Read(d.peer, one[:])
		switch {
		case err == nil && n == 0:
			return "EOF"
		case err == nil:
			return "data"
		case !errors.Is(err, unix.EAGAIN):
			return err.Error()
		}
		if time.Now().After(deadline) {
			return "open"
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (d *testDriver) closePeer() {
	if !d.peerClosed {
		_ = unix.Close(d.peer)
		d.peerClosed = true
	}
}

// expectReleased asserts what a caller that unregistered and closed fd is
// owed: onClose fires, and the socket is closed, so its peer reads EOF.
func (d *testDriver) expectReleased(t *testing.T, what string) {
	t.Helper()
	fired, err := d.waitClosed()
	d.closeErr = err
	peer := d.peerSees()
	if !fired || peer != "EOF" {
		t.Errorf("%s: 2 s later onClose fired=%v and the peer reads %s; want onClose and EOF (celeris#691)",
			what, fired, peer)
	}
}

// expectReceives asserts d's RECV is still armed: a byte from its peer
// reaches onRecv.
func (d *testDriver) expectReceives(t *testing.T, what string) {
	t.Helper()
	if _, err := unix.Write(d.peer, []byte{'x'}); err != nil {
		t.Fatalf("write to peer: %v", err)
	}
	select {
	case <-d.recv:
	case <-time.After(2 * time.Second):
		t.Errorf("%s: a byte from the peer never reached onRecv: its RECV was cancelled", what)
	}
}

// unregisterAndWait tears d down in the safe order and closes both ends.
func (d *testDriver) unregisterAndWait(t *testing.T, wl engine.WorkerLoop) {
	t.Helper()
	if err := wl.UnregisterConn(d.fd); err != nil {
		t.Errorf("UnregisterConn(%d): %v", d.fd, err)
	}
	if fired, _ := d.waitClosed(); !fired {
		t.Errorf("fd %d: onClose never fired", d.fd)
	}
	_ = unix.Close(d.fd)
	d.closePeer()
}

// workerPark parks a worker's goroutine inside a driver onRecv callback, so
// driver actions queued meanwhile are applied only after release, and runs
// functions on that goroutine on request.
type workerPark struct {
	t       *testing.T
	d       *testDriver
	entered chan chan func()
	run     chan func()
}

func newWorkerPark(t *testing.T, w *Worker, wl engine.WorkerLoop) *workerPark {
	t.Helper()
	p := &workerPark{t: t, entered: make(chan chan func())}
	d := &testDriver{closed: make(chan error, 1)}
	d.fd, d.peer = nonblockSocketPair(t)
	if err := wl.RegisterConn(d.fd, func([]byte) {
		run := make(chan func())
		p.entered <- run
		for f := range run {
			f()
		}
	}, func(err error) { d.closed <- err }); err != nil {
		t.Fatalf("RegisterConn(parker): %v", err)
	}
	p.d = d
	settleDriverRecv(t, w, d.fd)
	// A test that fails while parked must not leave the worker parked.
	t.Cleanup(func() {
		if p.run != nil {
			close(p.run)
			p.run = nil
		}
	})
	return p
}

// park returns once the worker goroutine is inside the parker's onRecv.
func (p *workerPark) park() {
	p.t.Helper()
	if _, err := unix.Write(p.d.peer, []byte{'p'}); err != nil {
		p.t.Fatalf("write to parker: %v", err)
	}
	select {
	case p.run = <-p.entered:
	case <-time.After(5 * time.Second):
		p.t.Fatal("the worker never entered the parking callback")
	}
}

// onWorker runs f on the parked worker goroutine and waits for it.
func (p *workerPark) onWorker(f func()) {
	done := make(chan struct{})
	p.run <- func() { f(); close(done) }
	<-done
}

func (p *workerPark) release() {
	close(p.run)
	p.run = nil
}

// R1, what every in-tree driver does: the RECV is armed, UnregisterConn, then
// Close at once. Before celeris#691 the worker's cancel resolved the closed
// number and missed.
func TestDriverUnregisterThenCloseAtOnce(t *testing.T) {
	e, stop := startTestEngine(t)
	t.Cleanup(stop)
	w, wl := e.workers[0], e.WorkerLoop(0)

	a := registerSettledDriver(t, w, wl)
	defer a.closePeer()
	if err := wl.UnregisterConn(a.fd); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	_ = unix.Close(a.fd)
	a.expectReleased(t, "UnregisterConn, then Close at once")
}

// R3: R1 made deterministic. The worker is parked while the caller
// unregisters and closes, so the cancel is issued after the close.
func TestDriverUnregisterThenCloseWhileWorkerBusy(t *testing.T) {
	e, stop := startTestEngine(t)
	t.Cleanup(stop)
	w, wl := e.workers[0], e.WorkerLoop(0)

	a := registerSettledDriver(t, w, wl)
	defer a.closePeer()
	p := newWorkerPark(t, w, wl)

	p.park()
	if err := wl.UnregisterConn(a.fd); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	_ = unix.Close(a.fd)
	p.release()
	a.expectReleased(t, "worker busy, UnregisterConn, then Close")
	p.d.unregisterAndWait(t, wl)
}

// R3's control: the same park, but the caller waits for onClose before it
// closes. It passes with or without the fix, so an R3 failure is about the
// order of the close and the cancel, not about the apparatus.
func TestDriverUnregisterWaitForOnCloseThenClose(t *testing.T) {
	e, stop := startTestEngine(t)
	t.Cleanup(stop)
	w, wl := e.workers[0], e.WorkerLoop(0)

	a := registerSettledDriver(t, w, wl)
	defer a.closePeer()
	p := newWorkerPark(t, w, wl)

	p.park()
	if err := wl.UnregisterConn(a.fd); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	p.release()
	fired, _ := a.waitClosed()
	_ = unix.Close(a.fd)
	if peer := a.peerSees(); !fired || peer != "EOF" {
		t.Errorf("UnregisterConn, wait for onClose, then Close: onClose fired=%v, peer reads %s; want onClose and EOF",
			fired, peer)
	}
	p.d.unregisterAndWait(t, wl)
}

// R1 with the number reused. Once the caller has closed fd, the next socket
// the process creates takes the number; here it is another driver conn on
// the same worker (dup3 makes the reuse deterministic). Before celeris#691
// the cancel resolved the number to THAT socket and cancelled its RECV,
// leaving it deaf, while the unregistered conn stayed open.
func TestDriverUnregisterThenCloseNumberReused(t *testing.T) {
	e, stop := startTestEngine(t)
	t.Cleanup(stop)
	w, wl := e.workers[0], e.WorkerLoop(0)

	a := registerSettledDriver(t, w, wl)
	defer a.closePeer()
	v := registerSettledDriver(t, w, wl)
	p := newWorkerPark(t, w, wl)

	p.park()
	if err := wl.UnregisterConn(a.fd); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	_ = unix.Close(a.fd)
	if err := unix.Dup3(v.fd, a.fd, unix.O_CLOEXEC); err != nil {
		t.Fatalf("dup3: %v", err)
	}
	p.release()
	a.expectReleased(t, "UnregisterConn, Close, number reused")
	v.expectReceives(t, "the socket that took the unregistered number")

	_ = unix.Close(a.fd) // v's second number
	v.unregisterAndWait(t, wl)
	p.d.unregisterAndWait(t, wl)
}

// The other fd-keyed cancel: failDriverConn, which handleDriverSend calls
// when a SEND fails while the RECV is still armed. It runs on the worker and
// queues its cancel; a caller that unregisters (a no-op by then) and closes
// before the next submit made that cancel miss too.
func TestDriverSendFailureThenUnregisterThenClose(t *testing.T) {
	e, stop := startTestEngine(t)
	t.Cleanup(stop)
	w, wl := e.workers[0], e.WorkerLoop(0)

	a := registerSettledDriver(t, w, wl)
	defer a.closePeer()
	p := newWorkerPark(t, w, wl)

	p.park()
	p.onWorker(func() {
		// What handleDriverSend does on a failed SEND, with the RECV armed.
		w.failDriverConn(driverConnOf(w, a.fd), errSendFailed)
	})
	if err := wl.UnregisterConn(a.fd); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	_ = unix.Close(a.fd)
	p.release()
	a.expectReleased(t, "SEND failure, then UnregisterConn and Close")
	if a.closeErr != nil && !errors.Is(a.closeErr, errSendFailed) {
		t.Errorf("onClose(%v), want the SEND's error", a.closeErr)
	}
	p.d.unregisterAndWait(t, wl)
}

// A register that armDriverRecv refuses (an HTTP conn took the number first)
// leaves the conn retired, with an UnregisterConn already queued behind it.
// That unregister must issue nothing: by the time it runs, the number may
// name another socket. Before celeris#691 it cancelled that socket's ops.
func TestDriverRefusedRegisterThenNumberReused(t *testing.T) {
	e, stop := startTestEngine(t)
	t.Cleanup(stop)
	w, wl := e.workers[0], e.WorkerLoop(0)

	v := registerSettledDriver(t, w, wl)
	p := newWorkerPark(t, w, wl)

	p.park()
	a := newTestDriver(t, wl) // queued behind the park
	defer a.closePeer()
	if err := wl.UnregisterConn(a.fd); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	_ = unix.Close(a.fd)
	fd := a.fd
	p.onWorker(func() { w.conns[fd] = &connState{fd: fd} }) // an HTTP conn took the number
	if err := unix.Dup3(v.fd, a.fd, unix.O_CLOEXEC); err != nil {
		t.Fatalf("dup3: %v", err)
	}
	p.release()
	if fired, err := a.waitClosed(); !fired || err == nil {
		t.Errorf("the refused register: onClose fired=%v with %v, want an error", fired, err)
	}
	v.expectReceives(t, "the socket that took the refused conn's number")

	p.park()
	p.onWorker(func() { w.conns[fd] = nil })
	p.release()
	_ = unix.Close(a.fd) // v's second number
	v.unregisterAndWait(t, wl)
	p.d.unregisterAndWait(t, wl)
}

// Closing fd before UnregisterConn is outside the contract. When the number
// has been reused by then, the engine must not act on the socket that now
// holds it: the duplicate would name that socket, and is refused.
func TestDriverCloseBeforeUnregisterSparesReusedNumber(t *testing.T) {
	e, stop := startTestEngine(t)
	t.Cleanup(stop)
	w, wl := e.workers[0], e.WorkerLoop(0)

	a := registerSettledDriver(t, w, wl)
	defer a.closePeer()
	v := registerSettledDriver(t, w, wl)
	p := newWorkerPark(t, w, wl)

	p.park()
	_ = unix.Close(a.fd)
	if err := unix.Dup3(v.fd, a.fd, unix.O_CLOEXEC); err != nil {
		t.Fatalf("dup3: %v", err)
	}
	if err := wl.UnregisterConn(a.fd); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	p.release()
	v.expectReceives(t, "the socket that took the number closed before UnregisterConn")

	// The unregistered conn cannot be cancelled (no descriptor names its
	// socket), but it is finalized once its armed RECV completes.
	a.closePeer()
	if fired, _ := a.waitClosed(); !fired {
		t.Error("closed before UnregisterConn: onClose never fired after the peer closed")
	}
	_ = unix.Close(a.fd) // v's second number
	v.unregisterAndWait(t, wl)
	p.d.unregisterAndWait(t, wl)
}

// fdTable maps every open descriptor of this process to what it names.
func fdTable(t *testing.T) map[int]string {
	t.Helper()
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	m := make(map[int]string, len(ents))
	for _, ent := range ents {
		n, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		link, err := os.Readlink("/proc/self/fd/" + ent.Name())
		if err != nil {
			continue // ReadDir's own descriptor, closed by now
		}
		m[n] = link
	}
	return m
}

func socketName(t *testing.T, fd int) string {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatalf("fstat(%d): %v", fd, err)
	}
	return fmt.Sprintf("socket:[%d]", st.Ino)
}

// expectSameFDTable fails if after holds more or fewer descriptors than
// before, or any descriptor naming one of sockets.
func expectSameFDTable(t *testing.T, before, after map[int]string, sockets map[string]bool) {
	t.Helper()
	for fd, name := range after {
		if sockets[name] {
			t.Errorf("fd %d still names %s, a socket every caller has closed", fd, name)
		}
	}
	if len(after) == len(before) {
		return
	}
	var added, gone []string
	for fd, name := range after {
		if before[fd] != name {
			added = append(added, fmt.Sprintf("%d=%s", fd, name))
		}
	}
	for fd, name := range before {
		if after[fd] != name {
			gone = append(gone, fmt.Sprintf("%d=%s", fd, name))
		}
	}
	t.Errorf("open descriptors: %d before, %d after; added [%s], gone [%s]",
		len(before), len(after), strings.Join(added, " "), strings.Join(gone, " "))
}

// Every path that ends a driver conn must close the duplicate it may hold:
// N cycles of each, and the process holds exactly the descriptors it held
// before, none of them naming a cycle's socket.
func TestDriverUnregisterCyclesReleaseDescriptors(t *testing.T) {
	const n = 8
	e, stop := startTestEngine(t)
	t.Cleanup(stop)
	w, wl := e.workers[0], e.WorkerLoop(0)
	p := newWorkerPark(t, w, wl)

	sockets := make(map[string]bool)
	before := fdTable(t)

	// parked: RegisterConn is queued while the worker is parked, so it is
	// applied together with what the body queues, after the body releases.
	cycle := func(kind string, i int, parked bool, body func(d *testDriver)) {
		t.Helper()
		if parked {
			p.park()
		}
		d := newTestDriver(t, wl)
		sockets[socketName(t, d.fd)] = true
		body(d)
		if t.Failed() {
			t.Fatalf("%s cycle %d failed", kind, i)
		}
	}
	for i := range n {
		// What the drivers do: armed, UnregisterConn, Close at once.
		cycle("unregister-close", i, false, func(d *testDriver) {
			settleDriverRecv(t, w, d.fd)
			_ = wl.UnregisterConn(d.fd)
			_ = unix.Close(d.fd)
			d.expectReleased(t, "unregister-close")
			d.closePeer()
		})
		// Register and unregister applied in one worker batch: no RECV is
		// ever armed.
		cycle("same-batch", i, true, func(d *testDriver) {
			_ = wl.UnregisterConn(d.fd)
			_ = unix.Close(d.fd)
			p.release()
			d.expectReleased(t, "same-batch")
			d.closePeer()
		})
		// The peer closes first: finalized on EOF, no duplicate taken.
		cycle("peer-eof", i, false, func(d *testDriver) {
			settleDriverRecv(t, w, d.fd)
			d.closePeer()
			if fired, _ := d.waitClosed(); !fired {
				t.Error("peer-eof: onClose never fired")
			}
			if err := wl.UnregisterConn(d.fd); err != nil && !errors.Is(err, engine.ErrUnknownFD) {
				t.Errorf("peer-eof: UnregisterConn: %v", err)
			}
			_ = unix.Close(d.fd)
		})
		// A SEND failure takes the duplicate on the worker.
		cycle("send-failure", i, false, func(d *testDriver) {
			settleDriverRecv(t, w, d.fd)
			p.park()
			p.onWorker(func() { w.failDriverConn(driverConnOf(w, d.fd), errSendFailed) })
			_ = wl.UnregisterConn(d.fd)
			_ = unix.Close(d.fd)
			p.release()
			d.expectReleased(t, "send-failure")
			d.closePeer()
		})
		// armDriverRecv refuses the register after UnregisterConn took the
		// duplicate.
		cycle("refused", i, true, func(d *testDriver) {
			fd := d.fd
			_ = wl.UnregisterConn(fd)
			_ = unix.Close(fd)
			p.onWorker(func() { w.conns[fd] = &connState{fd: fd} })
			p.release()
			if fired, err := d.waitClosed(); !fired || err == nil {
				t.Errorf("refused: onClose fired=%v with %v, want an error", fired, err)
			}
			p.park()
			p.onWorker(func() { w.conns[fd] = nil })
			p.release()
			d.closePeer()
		})
	}
	expectSameFDTable(t, before, fdTable(t), sockets)
	p.d.unregisterAndWait(t, wl)
}

// Shutdown with unregisters queued and their cancels never completed: the
// duplicates close with the worker, and every socket is released.
func TestDriverShutdownReleasesDescriptors(t *testing.T) {
	const n = 8
	// Anything the runtime opens on its first network use is open before
	// the first snapshot, not counted as a leak after the last.
	if ln, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
		_ = ln.Close()
	}
	before := fdTable(t)

	e, stop := startTestEngine(t)
	t.Cleanup(stop) // idempotent; the test calls it itself below
	w, wl := e.workers[0], e.WorkerLoop(0)
	sockets := make(map[string]bool)
	ds := make([]*testDriver, 0, n)
	for range n {
		d := registerSettledDriver(t, w, wl)
		sockets[socketName(t, d.fd)] = true
		ds = append(ds, d)
	}
	p := newWorkerPark(t, w, wl)

	p.park()
	for _, d := range ds {
		_ = wl.UnregisterConn(d.fd)
		_ = unix.Close(d.fd)
	}
	// Cancel the engine while the worker is parked: on release it prepares
	// the cancels, then its next iteration sees the context and shuts down
	// before issuing them.
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	for w.runCtx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	p.release()
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("the engine did not stop")
	}

	for i, d := range ds {
		if fired, err := d.waitClosed(); !fired || !errors.Is(err, errEngineShutdown) {
			t.Errorf("conn %d: onClose fired=%v with %v, want errEngineShutdown", i, fired, err)
		}
		if peer := d.peerSees(); peer != "EOF" {
			t.Errorf("conn %d: after shutdown the peer reads %s, want EOF", i, peer)
		}
		d.closePeer()
	}
	_ = unix.Close(p.d.fd)
	p.d.closePeer()
	expectSameFDTable(t, before, fdTable(t), sockets)
}
