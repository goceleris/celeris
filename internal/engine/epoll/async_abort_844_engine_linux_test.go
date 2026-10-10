//go:build linux

package epoll

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

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

func unavailable844(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Fatalf(format, args...) // not a skip: a skip would take the witness out of CI silently
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
