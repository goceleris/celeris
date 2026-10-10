//go:build linux

package epoll

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// The epoll half of the celeris#844 rig (async_abort_844_linux_test.go is the
// same file on both engines).

const (
	engineName844 = "epoll"
	pkgDir844     = "epoll"
)

func tier844(*Engine) string { return "n/a" }

// parkRouteReachable844: epoll's park loop runs no application code under
// asyncInMu (askAtPark is engine code), so there is no route-resolver arm.
func parkRouteReachable844(*Engine) (bool, string) {
	return false, "epoll's park loop runs only engine code under asyncInMu (askAtPark)"
}

// retryStart844: epoll has no ring budget to wait for.
func retryStart844(start func() error) error { return start() }

// unavailable844: not a skip, a skip would take the witness out of CI silently.
func unavailable844(t *testing.T, err error) {
	t.Helper()
	t.Fatalf("epoll engine did not start: %v", err)
}

// Item 4, the peer-FIN interleaving, deterministically: the handler is running
// when the peer's FIN reaches the loop, so closeConn finds detachMu held with
// the goroutine busy and leaves the close to it (closeOwed). Then the handler
// panics or calls runtime.Goexit; the abort must release the lock and hand the
// conn back, and the loop's drain must close it, once. The wait for "the FIN
// was handled" is the condition cs.closeOwed under asyncInMu, not a sleep (the
// review's scratch arm slept 300 ms).

type holdHandler844 struct {
	how     string
	entered chan struct{}
	release chan struct{}
}

func (h *holdHandler844) HandleStream(_ context.Context, _ *stream.Stream) error {
	h.entered <- struct{}{}
	<-h.release
	if h.how == "goexit" {
		runtime.Goexit()
	}
	panic("celeris844: handler panic after the peer closed")
}
func (*holdHandler844) RouteAsync(_, _ string) bool { return true }
func (*holdHandler844) HasAsyncRoutes() bool        { return true }

func TestAbortAfterPeerCloseHandsTheConnBack844(t *testing.T) {
	for _, how := range []string{"panic", "goexit"} {
		t.Run(how, func(t *testing.T) {
			rig := hijackRaceConn(t)
			l, cs, local, peer := rig.l, rig.cs, rig.local, rig.peer
			l.async = true
			cs.buf = make([]byte, 4096)
			cs.ctx = context.Background()
			cs.writeFn = func([]byte) {}
			h := &holdHandler844{how: how, entered: make(chan struct{}, 1), release: make(chan struct{})}
			l.handler = h
			var once sync.Once
			rel := func() { once.Do(func() { close(h.release) }) }
			t.Cleanup(rel)
			cs.asyncInMu.Lock()
			cs.asyncInBuf = append(cs.asyncInBuf, "GET /hold HTTP/1.1\r\nHost: x\r\n\r\n"...)
			cs.asyncRun = true
			cs.asyncInMu.Unlock()
			l.asyncWG.Add(1)
			go l.runAsyncHandler(cs)
			select {
			case <-h.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("PREMISE: the handler never started")
			}
			if err := unix.Shutdown(peer, unix.SHUT_WR); err != nil {
				t.Fatalf("shutdown peer: %v", err)
			}
			l.drainRead(local, time.Now().UnixNano()) // the peer's FIN
			cs.asyncInMu.Lock()
			owed := cs.closeOwed
			cs.asyncInMu.Unlock()
			if !owed || l.conns[local] != cs {
				t.Fatalf("PREMISE: after the FIN closeOwed=%v slot-kept=%v, want the close left to the running handler",
					owed, l.conns[local] == cs)
			}
			rel()
			exited := make(chan struct{})
			go func() { l.asyncWG.Wait(); close(exited) }()
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				t.Fatalf("celeris#844: the dispatch goroutine did not exit after its handler ended (%s)", how)
			}
			if !cs.detachMu.TryLock() {
				t.Fatalf("celeris#844: detachMu is still held after the dispatch goroutine exited (%s)", how)
			}
			cs.detachMu.Unlock()
			l.drainDetachQueue()
			if l.conns[local] != nil || l.closeCount.Load() != 1 || rig.disconnects.Load() != 1 {
				t.Errorf("celeris#844: the owed close did not run exactly once after the abort (%s): slot=%p closeCount=%d hooks=%d",
					how, l.conns[local], l.closeCount.Load(), rig.disconnects.Load())
			}
			t.Logf("celeris844 RESULT engine=epoll arm=peer-fin-then-%s closeOwed_before_release=%v closeCount=%d slot_cleared=%v",
				how, owed, l.closeCount.Load(), l.conns[local] == nil)
		})
	}
}

// takeTarget844 takes a handed-off descriptor, closing it, and counts it.
type takeTarget844 struct{ n atomic.Int64 }

func (t *takeTarget844) AdoptConn(fd int, _ engine.Carryover) error {
	t.n.Add(1)
	_ = unix.Close(fd)
	return nil
}

// TestTransplantSweepLeavesAConnWhoseGoroutineExitedToCloseIt844: the sweep's
// tryTransplant checks asyncClosed before it takes asyncInMu, and a dispatch
// goroutine that aborts sets it and then clears asyncRun (endDispatch) in the
// time between. Past the lock the conn then read as one with no goroutine, and
// was handed off: its connState released, a refused hand-off reclaimed as a
// new conn that outlived the close the abort asked for (the celeris#844
// park-window arm saw it, about one run in fifty). The io_uring sweep reads
// asyncClosed under the lock (celeris#780). The test holds the lock, lets the
// sweep reach it, and then plays the abort: asyncClosed, then asyncRun cleared.
func TestTransplantSweepLeavesAConnWhoseGoroutineExitedToCloseIt844(t *testing.T) {
	rig := hijackRaceConn(t)
	l, cs, local := rig.l, rig.cs, rig.local
	l.async = true
	cs.protocol = engine.HTTP1
	cs.detected = true
	tgt := &takeTarget844{}
	l.transplant.Store(&transplantState{target: tgt})

	cs.asyncInMu.Lock()
	cs.asyncParked = true // as in the park loop: the goroutine holds the lock, parked, input empty
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.tryTransplant(local)
	}()
	// The sweep has passed its early check and waits for the lock: a condition
	// on the stacks, not a sleep.
	blocked := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		buf := make([]byte, 1<<20)
		buf = buf[:runtime.Stack(buf, true)]
		for _, g := range strings.Split(string(buf), "\n\n") {
			if strings.Contains(g, ".tryTransplant(") && strings.Contains(g, "sync.(*Mutex).Lock") {
				blocked = true
			}
		}
		if blocked {
			break
		}
	}
	if !blocked {
		cs.asyncInMu.Unlock()
		t.Fatal("PREMISE: tryTransplant never blocked on asyncInMu")
	}
	// The abort: asyncClosed first, then the goroutine is gone.
	cs.asyncClosed.Store(true)
	cs.asyncRun = false
	cs.asyncParked = false
	cs.asyncInMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tryTransplant did not return")
	}
	t.Logf("celeris844 RESULT engine=epoll arm=sweep-vs-abort handed_off=%d slot_kept=%v", tgt.n.Load(), l.conns[local] == cs)
	if tgt.n.Load() != 0 || l.conns[local] != cs {
		t.Errorf("celeris#844: the transplant sweep handed off a conn whose dispatch goroutine had exited to close it: "+
			"AdoptConn calls=%d slot-kept=%v", tgt.n.Load(), l.conns[local] == cs)
	}
}

// TestCloseOfADetachedConnTheDrainNeverCounted844: a conn whose OnDetach ran on
// its dispatch goroutine but whose increment of detachedCount (the drain's, on
// the loop thread) has not run is closed without ever having been counted,
// which is where a panic inside OnDetach's window leaves it (celeris#844): the
// abort's hand-back reaches the drain's asyncClosed branch first. Its close
// must not lower the count for another detached conn that was counted. The
// control: a counted conn's close lowers it once.
func TestCloseOfADetachedConnTheDrainNeverCounted844(t *testing.T) {
	for _, tc := range []struct {
		name    string
		counted bool
		want    int
	}{
		{"never-counted", false, 1},
		{"counted-control", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := hijackRaceConn(t)
			l, cs, local := rig.l, rig.cs, rig.local
			l.async = true
			l.detachedCount = 1 // another detached conn is live on this loop
			cs.asyncRun = false
			cs.asyncPromoted = true
			cs.h1State.Detached.Store(true) // OnDetach's window panic publishes it
			cs.asyncDetachPending = !tc.counted
			cs.detachCounted = tc.counted
			l.closeConn(local)
			t.Logf("celeris844 RESULT engine=epoll arm=detached-accounting case=%s detachedCount=%d want=%d closeCount=%d",
				tc.name, l.detachedCount, tc.want, l.closeCount.Load())
			if l.closeCount.Load() != 1 {
				t.Fatalf("PREMISE: closeConn did not close the conn (closeCount=%d)", l.closeCount.Load())
			}
			if l.detachedCount != tc.want {
				t.Errorf("celeris#844: after the close of a detached conn that was counted=%v, detachedCount=%d, want %d "+
					"(another detached conn's place was taken)", tc.counted, l.detachedCount, tc.want)
			}
		})
	}
}
