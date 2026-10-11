//go:build linux

package epoll

// celeris#930: the engine loop's read of a driver conn. driverRead read a
// conn until a read came back shorter than the buffer, with no bound on the
// number of reads, so a conn whose peer kept its socket full held the loop and
// every other conn on it (driver or HTTP) waited. The engine twin of
// celeris#881, which fixed the same loop shape in the standalone driver loop.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestADriverConnWithEndlessInflowDoesNotStarveTheOthers930: A's peer writes
// 64 KiB chunks as fast as A's socket takes them, and A's onRecv sleeps 100
// us a chunk, so A's socket never empties. B, a second driver conn on the same
// loop, is sent one byte. B must be served within 3 s while A's inflow goes on.
func TestADriverConnWithEndlessInflowDoesNotStarveTheOthers930(t *testing.T) {
	eng, stop := newTestEngine(t)
	defer stop()
	wl := eng.WorkerLoop(0)
	a, aPeer := socketpairNonblocking(t)
	b, bPeer := socketpairNonblocking(t)
	defer func() {
		_ = wl.UnregisterConn(a)
		_ = wl.UnregisterConn(b)
		for _, fd := range []int{a, aPeer, b, bPeer} {
			_ = unix.Close(fd)
		}
	}()
	var aGot, aReads atomic.Int64
	aFirst := make(chan struct{}, 1)
	if err := wl.RegisterConn(a, func(p []byte) {
		aReads.Add(1)
		if aGot.Add(int64(len(p))) == int64(len(p)) {
			aFirst <- struct{}{}
		}
		time.Sleep(100 * time.Microsecond)
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}
	var aReadsAtB atomic.Int64
	bGot := make(chan time.Time, 1)
	if err := wl.RegisterConn(b, func([]byte) {
		aReadsAtB.Store(aReads.Load())
		select {
		case bGot <- time.Now():
		default:
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(B): %v", err)
	}
	stopFeed := make(chan struct{})
	var fed atomic.Int64
	var feeder sync.WaitGroup
	feeder.Add(1)
	go func() {
		defer feeder.Done()
		chunk := make([]byte, 64<<10)
		for {
			select {
			case <-stopFeed:
				return
			default:
			}
			n, err := unix.Write(aPeer, chunk)
			if n > 0 {
				fed.Add(int64(n))
			}
			if err == unix.EAGAIN {
				time.Sleep(10 * time.Microsecond)
				continue
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-aFirst:
	case <-time.After(5 * time.Second):
		close(stopFeed)
		feeder.Wait()
		t.Fatal("A's onRecv never ran")
	}
	time.Sleep(20 * time.Millisecond)
	readsAtSend := aReads.Load()
	sent := time.Now()
	if _, err := unix.Write(bPeer, []byte{'b'}); err != nil {
		t.Fatalf("write B's peer: %v", err)
	}
	var at time.Time
	served := false
	select {
	case at = <-bGot:
		served = true
	case <-time.After(3 * time.Second):
	}
	close(stopFeed)
	feeder.Wait()
	if !served {
		select {
		case at = <-bGot:
		case <-time.After(5 * time.Second):
		}
	}
	t.Logf("C930 flood: A fed %d bytes, A's onRecv got %d in %d reads; B served while A's inflow went on: %v (after %v, A read %d chunks in between)",
		fed.Load(), aGot.Load(), aReads.Load(), served, at.Sub(sent), aReadsAtB.Load()-readsAtSend)
	if !served {
		t.Errorf("B, a driver conn on the same engine loop, was not served within 3 s while A's peer kept A's socket full")
	}
}
