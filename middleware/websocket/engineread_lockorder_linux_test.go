//go:build linux

package websocket

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/wakefd"
)

// This file checks the lock order behind celeris#667's fix after celeris#666.
//
// The #667 fix runs the engine's pause/resume callbacks with pausedMu held.
// #666 then made those callbacks wake the loop through a wakefd.WakeFD, whose
// Signal takes a sync.RWMutex READ lock. So pausedMu now nests a read lock of
// an RWMutex whose WRITERS (Set and Close) run on the loop thread. A reader
// queued behind a waiting writer cannot get in, so if a writer could ever
// wait on pausedMu, a callback holding pausedMu and the writer would wait on
// each other forever.
//
// The test below forces the interleavings where that would show, with the
// production WakeFD and callbacks built like the engines' own, and requires
// each to complete while a callback holds pausedMu. A watchdog turns a
// deadlock into a failure instead of a hung binary.

// lockOrderWatchdog bounds every wait below. Each wait is for a goroutine to
// reach a state it reaches with no timing involved, so the bound only turns a
// deadlock into a failure; it is not a performance assertion.
const lockOrderWatchdog = 30 * time.Second

// parkPoint parks a callback at one instant: after it has released detachQMu
// and before it calls Signal, i.e. with pausedMu held and the WakeFD's read
// lock not yet requested.
type parkPoint struct {
	parked  chan struct{}
	release chan struct{}
}

// wakeShapedEngine builds its pause/resume callbacks the way both engines
// build PauseRecv/ResumeRecv on detach (engine/iouring/worker.go and
// engine/epoll/loop.go): a Swap of the desired state that returns early on a
// no-op, an append to the detach queue under detachQMu, and, only when that
// append took the queue from empty to non-empty, a Signal of the loop's
// WakeFD after detachQMu is released. The WakeFD is the production type, so
// the RWMutex under test is the real one.
type wakeShapedEngine struct {
	desired atomic.Bool  // connState.recvPauseDesired
	pending atomic.Int32 // detachQPending
	qmu     sync.Mutex   // detachQMu
	queue   []int        // detachQueue
	wake    *wakefd.WakeFD
	park    atomic.Pointer[parkPoint]
	signals atomic.Int64 // Signal calls the callbacks made
}

func (e *wakeShapedEngine) pause()  { e.apply(true) }
func (e *wakeShapedEngine) resume() { e.apply(false) }

func (e *wakeShapedEngine) apply(want bool) {
	if e.desired.Swap(want) == want {
		return
	}
	e.qmu.Lock()
	e.queue = append(e.queue, 1)
	wasEmpty := e.pending.Swap(1) == 0
	e.qmu.Unlock()
	if p := e.park.Swap(nil); p != nil {
		close(p.parked)
		<-p.release
	}
	if wasEmpty {
		e.signals.Add(1)
		e.wake.Signal()
	}
}

// drain is drainDetachQueue's queue swap: it re-arms the empty->non-empty
// edge, so the next callback signals.
func (e *wakeShapedEngine) drain() {
	e.qmu.Lock()
	e.queue = e.queue[:0]
	e.pending.Store(0)
	e.qmu.Unlock()
}

// armPark makes the next callback that reaches Signal park first.
func (e *wakeShapedEngine) armPark() *parkPoint {
	p := &parkPoint{parked: make(chan struct{}), release: make(chan struct{})}
	e.park.Store(p)
	return p
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(lockOrderWatchdog):
		t.Fatalf("no progress in %v: %s", lockOrderWatchdog, what)
	}
}

func requirePausedMuHeld(t *testing.T, r *chanReader, when string) {
	t.Helper()
	if r.pausedMu.TryLock() {
		r.pausedMu.Unlock()
		t.Fatalf("harness: pausedMu is free %s; the callback was supposed to be holding it", when)
	}
}

func requirePausedMuFree(t *testing.T, r *chanReader, when string) {
	t.Helper()
	if !r.pausedMu.TryLock() {
		t.Fatalf("pausedMu is still held %s", when)
	}
	r.pausedMu.Unlock()
}

// eventfdCount reads and resets an eventfd counter through a descriptor that
// shares it; 0 means nothing was written.
func eventfdCount(t *testing.T, fd int) uint64 {
	t.Helper()
	var b [8]byte
	n, err := unix.Read(fd, b[:])
	if errors.Is(err, unix.EAGAIN) {
		return 0
	}
	if err != nil || n != 8 {
		t.Fatalf("read eventfd: n=%d err=%v", n, err)
	}
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}

// waitGoroutine polls the goroutine dump until one goroutine's stack contains
// every marker. It reports whether one did before stop closed.
func waitGoroutine(stop <-chan struct{}, markers ...string) bool {
	deadline := time.Now().Add(lockOrderWatchdog)
	buf := make([]byte, 1<<20)
	for time.Now().Before(deadline) {
		n := runtime.Stack(buf, true)
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			all := true
			for _, m := range markers {
				if !strings.Contains(g, m) {
					all = false
					break
				}
			}
			if all {
				return true
			}
		}
		select {
		case <-stop:
			return false
		default:
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// inFlightSignal is a producer inside Signal: it holds the WakeFD's read lock
// across its write(2). A named function, so the goroutine dump can find it.
//
//go:noinline
func inFlightSignal(w *wakefd.WakeFD) { w.Signal() }

// loopShutdownClose is the loop thread closing its WakeFD at shutdown.
//
//go:noinline
func loopShutdownClose(w *wakefd.WakeFD) { w.Close() }

// TestChanReaderWakeFDWritersNeverWaitOnPausedMu forces three interleavings,
// one per way a WakeFD writer and a callback holding pausedMu can meet:
//
//   - Close-while-pause-holds-pausedMu: the engine worker's pause, applied in
//     requestPause with pausedMu held, is parked just before its Signal; the
//     loop's Close must complete while pausedMu stays held, and the Signal
//     that follows must return as a no-op.
//   - Set-while-resume-holds-pausedMu: the handler's resume, applied in
//     resumeIfDrained with pausedMu held, is parked just before its Signal on
//     a WakeFD with no descriptor yet (epoll creates its eventfd lazily); the
//     loop's Set must complete while pausedMu stays held, and the Signal that
//     follows must land on the descriptor Set installed.
//   - Close-queued-behind-in-flight-Signal: the case the RWMutex adds. Another
//     producer is inside Signal's write(2) holding the read lock, the loop's
//     Close is queued for the write lock behind it, and only then does the
//     pause callback, holding pausedMu, call Signal, which sync.RWMutex queues
//     behind the waiting writer. Every lock in the chain is then held or
//     awaited at once. It must drain once the in-flight write completes. To
//     hold a reader in write(2) for as long as the test needs, the WakeFD is
//     given a full pipe whose O_NONBLOCK the test clears after New set it: a
//     longer read hold than production can produce, not a shorter one.
func TestChanReaderWakeFDWritersNeverWaitOnPausedMu(t *testing.T) {
	newEventfd := func(t *testing.T) (fd, peek int) {
		t.Helper()
		fd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
		if err != nil {
			t.Fatalf("eventfd: %v", err)
		}
		// A second descriptor on the same counter, so the counter can be read
		// after Close has closed fd.
		peek, err = unix.Dup(fd)
		if err != nil {
			t.Fatalf("dup: %v", err)
		}
		t.Cleanup(func() { _ = unix.Close(peek) })
		return fd, peek
	}

	t.Run("Close-while-pause-holds-pausedMu", func(t *testing.T) {
		efd, peek := newEventfd(t)
		e := &wakeShapedEngine{wake: wakefd.New(efd)}
		t.Cleanup(e.wake.Close)
		r := newChanReader(8, 0, 0) // highWater 6, lowWater 2
		r.SetPauser(e.pause, e.resume)

		p := e.armPark()
		appended := make(chan struct{})
		go func() { // the engine worker
			defer close(appended)
			for range r.highWater {
				r.Append([]byte{'a'})
			}
		}()
		waitClosed(t, p.parked, "the pause callback never reached its Signal")
		requirePausedMuHeld(t, r, "with the pause callback parked")

		closed := make(chan struct{})
		go func() { defer close(closed); loopShutdownClose(e.wake) }()
		select {
		case <-closed:
		case <-time.After(lockOrderWatchdog):
			close(p.release)
			t.Fatalf("WakeFD.Close did not complete in %v while a pause callback held pausedMu: "+
				"a WakeFD writer waited on pausedMu", lockOrderWatchdog)
		}
		requirePausedMuHeld(t, r, "after Close returned")

		close(p.release)
		waitClosed(t, appended, "requestPause did not return after Close: its Signal blocked on a closed WakeFD")
		requirePausedMuFree(t, r, "after requestPause returned")
		if got := e.signals.Load(); got != 1 {
			t.Fatalf("harness: %d Signal calls, want 1", got)
		}
		if got := eventfdCount(t, peek); got != 0 {
			t.Fatalf("a Signal made after Close wrote %d to the closed descriptor's counter", got)
		}
		if !e.desired.Load() || !readerPaused(r) {
			t.Fatalf("harness: engine paused=%v reader paused=%v, want both paused", e.desired.Load(), readerPaused(r))
		}
	})

	t.Run("Set-while-resume-holds-pausedMu", func(t *testing.T) {
		e := &wakeShapedEngine{wake: wakefd.New(-1)}
		t.Cleanup(e.wake.Close)
		r := newChanReader(8, 0, 0)
		r.SetPauser(e.pause, e.resume)
		for range r.highWater {
			r.Append([]byte{'a'})
		}
		if !e.desired.Load() {
			t.Fatal("harness: not paused after crossing highWater")
		}
		e.drain() // the loop drained the pause, so the resume signals

		p := e.armPark()
		read := make(chan struct{})
		go func() { // the handler goroutine
			defer close(read)
			buf := make([]byte, 1)
			for range r.highWater - r.lowWater {
				_, _ = r.Read(buf)
			}
		}()
		waitClosed(t, p.parked, "the resume callback never reached its Signal")
		requirePausedMuHeld(t, r, "with the resume callback parked")

		efd, peek := newEventfd(t)
		set := make(chan bool, 1)
		go func() { set <- e.wake.Set(efd) }() // the loop creating its eventfd lazily
		select {
		case ok := <-set:
			if !ok {
				t.Fatal("harness: Set refused on an open WakeFD")
			}
		case <-time.After(lockOrderWatchdog):
			close(p.release)
			t.Fatalf("WakeFD.Set did not complete in %v while a resume callback held pausedMu: "+
				"a WakeFD writer waited on pausedMu", lockOrderWatchdog)
		}
		requirePausedMuHeld(t, r, "after Set returned")

		close(p.release)
		waitClosed(t, read, "Read did not return after Set: the resume callback's Signal blocked")
		requirePausedMuFree(t, r, "after resumeIfDrained returned")
		if got := eventfdCount(t, peek); got != 1 {
			t.Fatalf("the resume's Signal after Set wrote %d to the new descriptor, want 1", got)
		}
		if e.desired.Load() || readerPaused(r) {
			t.Fatalf("harness: engine paused=%v reader paused=%v, want both running", e.desired.Load(), readerPaused(r))
		}
	})

	t.Run("Close-queued-behind-in-flight-Signal", func(t *testing.T) {
		var fds [2]int
		if err := unix.Pipe2(fds[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
			t.Fatalf("pipe: %v", err)
		}
		pr, pw := fds[0], fds[1]
		t.Cleanup(func() { _ = unix.Close(pr) })
		// Fill the pipe, so the next write(2) to it has to wait for a reader.
		chunk := make([]byte, 4096)
		for {
			if _, err := unix.Write(pw, chunk); err != nil {
				break
			}
		}
		for {
			if _, err := unix.Write(pw, chunk[:8]); err != nil {
				break
			}
		}
		e := &wakeShapedEngine{wake: wakefd.New(pw)}
		t.Cleanup(e.wake.Close)
		flags, err := unix.FcntlInt(uintptr(pw), unix.F_GETFL, 0)
		if err != nil {
			t.Fatalf("fcntl: %v", err)
		}
		if _, err := unix.FcntlInt(uintptr(pw), unix.F_SETFL, flags&^unix.O_NONBLOCK); err != nil {
			t.Fatalf("fcntl: %v", err)
		}
		drainPipe := func() { // until empty (EAGAIN) or, once Close has run, EOF
			buf := make([]byte, 1<<16)
			for {
				if n, err := unix.Read(pr, buf); err != nil || n == 0 {
					return
				}
			}
		}
		if err := unix.SetNonblock(pr, true); err != nil {
			t.Fatalf("set nonblock: %v", err)
		}
		stop := make(chan struct{})
		t.Cleanup(func() { close(stop); drainPipe() })

		r := newChanReader(8, 0, 0)
		r.SetPauser(e.pause, e.resume)

		// 1. The engine worker's pause, parked with pausedMu held.
		p := e.armPark()
		appended := make(chan struct{})
		go func() {
			defer close(appended)
			for range r.highWater {
				r.Append([]byte{'a'})
			}
		}()
		waitClosed(t, p.parked, "the pause callback never reached its Signal")

		// 2. Another producer inside Signal's write(2), holding the read lock.
		signalled := make(chan struct{})
		go func() { defer close(signalled); inFlightSignal(e.wake) }()
		if !waitGoroutine(stop, "websocket.inFlightSignal", "wakefd.(*WakeFD).Signal", "[syscall") {
			close(p.release)
			t.Fatal("harness: the in-flight Signal never blocked in write(2)")
		}

		// 3. The loop's Close, queued for the write lock behind it.
		closed := make(chan struct{})
		go func() { defer close(closed); loopShutdownClose(e.wake) }()
		if !waitGoroutine(stop, "websocket.loopShutdownClose", "sync.(*RWMutex).Lock") {
			close(p.release)
			t.Fatal("harness: Close never queued for the write lock")
		}

		// 4. The parked pause calls Signal, holding pausedMu. With a writer
		// waiting, sync.RWMutex queues this read lock behind it.
		close(p.release)
		if !waitGoroutine(stop, "(*chanReader).requestPause", "sync.(*RWMutex).RLock") {
			t.Fatal("harness: the pause callback's Signal did not queue behind the waiting Close " +
				"(sync.RWMutex no longer blocks readers while a writer waits?)")
		}
		requirePausedMuHeld(t, r, "with the pause callback queued behind Close")

		// Every lock in the chain is now held or awaited. Only the in-flight
		// write(2) can move, and nothing it waits for involves pausedMu.
		drainPipe()
		waitClosed(t, signalled, "the in-flight Signal did not finish once the pipe drained")
		waitClosed(t, closed, "Close did not complete after the in-flight Signal released the read lock: "+
			"a WakeFD writer waited on pausedMu")
		waitClosed(t, appended, "requestPause did not return after Close: deadlock through pausedMu")
		requirePausedMuFree(t, r, "after requestPause returned")
		if !e.desired.Load() || !readerPaused(r) {
			t.Fatalf("harness: engine paused=%v reader paused=%v, want both paused", e.desired.Load(), readerPaused(r))
		}
	})
}
