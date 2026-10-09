//go:build linux

package eventloop

// celeris#881: the worker reads one conn until EAGAIN before it serves the
// next event. A conn whose inflow does not pause kept the worker in its read
// loop, and every other conn on the worker waited for it. The worker now
// spends at most readBudget reads on a conn per turn and queues a conn that
// still has bytes for another turn after the rest of the batch.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestAConnWithEndlessInflowDoesNotStarveTheOthers881: A's peer writes as
// fast as A's socket takes bytes, and A's onRecv is slow (100 us a chunk), so
// A's socket never empties while the feeder runs. B, on the same worker, is
// sent one byte; it must be served while A's inflow goes on. The feeder stops
// once B is served, or after 3 s. The bound does not depend on timing: the
// test counts A's reads between B's byte and B's onRecv. B's event is
// collected by the first epoll_wait after the turn of A in progress, so at
// most one turn of A's reads comes first; the test allows two (32 reads at
// a budget of 16).
func TestAConnWithEndlessInflowDoesNotStarveTheOthers881(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)

	a, aPeer := socketPair(t)
	b, bPeer := socketPair(t)
	t.Cleanup(func() {
		_ = w.UnregisterConn(a)
		_ = w.UnregisterConn(b)
		for _, fd := range []int{a, aPeer, b, bPeer} {
			_ = unix.Close(fd)
		}
	})
	var aGot, aReads atomic.Int64
	aFirst := make(chan struct{}, 1)
	if err := w.RegisterConn(a, func(p []byte) {
		aReads.Add(1)
		if aGot.Add(int64(len(p))) == int64(len(p)) {
			aFirst <- struct{}{}
		}
		time.Sleep(100 * time.Microsecond)
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}
	bGot := make(chan time.Time, 1)
	var aReadsAtB atomic.Int64
	if err := w.RegisterConn(b, func([]byte) {
		aReadsAtB.Store(aReads.Load()) // on the worker, the goroutine that counts A's reads
		select {
		case bGot <- time.Now():
		default:
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(B): %v", err)
	}

	stop := make(chan struct{})
	var fed atomic.Int64
	var feeder sync.WaitGroup
	feeder.Add(1)
	go func() {
		defer feeder.Done()
		chunk := make([]byte, 64<<10)
		for {
			select {
			case <-stop:
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
		close(stop)
		feeder.Wait()
		t.Fatal("A's onRecv never ran: the test cannot load the worker")
	}
	time.Sleep(20 * time.Millisecond) // the worker is deep in A's inflow
	readsAtSend := aReads.Load()
	sent := time.Now()
	c784Queue(t, bPeer, []byte{'b'})
	var servedAt time.Time
	served := false
	select {
	case servedAt = <-bGot:
		served = true
	case <-time.After(3 * time.Second):
	}
	close(stop)
	feeder.Wait()
	if !served {
		select {
		case servedAt = <-bGot:
		case <-time.After(5 * time.Second):
		}
	}
	wait := servedAt.Sub(sent)
	between := aReadsAtB.Load() - readsAtSend
	t.Logf("C881 starvation: A fed %d bytes, A's onRecv got %d; B served while A's inflow went on: %v (after %v, %d of A's reads after B's byte)", fed.Load(), aGot.Load(), served, wait, between)
	if !served {
		t.Errorf("B was not served within 3 s while A's peer kept A's socket full: the worker read A until EAGAIN (B served %v after its byte, once A's inflow stopped)", wait)
	} else if between > 32 {
		t.Errorf("B was served after %d of A's reads: more than two read turns of A came before B's event", between)
	}
}

// TestBudgetedReadsDeliverEveryByteInOrder881: a conn that has more bytes
// waiting than one read turn takes, and no further inflow, must still get
// every byte, in order: edge-triggered epoll reports no new edge for bytes
// already in the socket, so the worker's own re-queue is all that reads the
// rest. The source is a pipe sized to 1 MiB, filled before the conn is
// registered, so the first event finds 64 reads' worth (four turns). Then the
// write end is closed: onClose must fire after the last byte.
func TestBudgetedReadsDeliverEveryByteInOrder881(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)

	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		t.Fatalf("pipe2: %v", err)
	}
	rd, wr := p[0], p[1]
	t.Cleanup(func() { _ = unix.Close(rd) })
	const size = 1 << 20
	if _, err := unix.FcntlInt(uintptr(wr), unix.F_SETPIPE_SZ, size); err != nil {
		_ = unix.Close(wr)
		t.Fatalf("F_SETPIPE_SZ %d: %v", size, err)
	}
	pattern := make([]byte, size)
	for i := range pattern {
		pattern[i] = byte(i % 251)
	}
	if n, err := unix.Write(wr, pattern); n != size || err != nil {
		_ = unix.Close(wr)
		t.Fatalf("fill the pipe: wrote %d of %d (%v)", n, size, err)
	}

	var mu sync.Mutex
	got, bad, calls := 0, -1, 0
	all := make(chan struct{})
	closed := make(chan error, 1)
	gotAtClose := -1
	if err := w.RegisterConn(rd, func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		for i, c := range b {
			if bad < 0 && c != byte((got+i)%251) {
				bad = got + i
			}
		}
		got += len(b)
		if got == size {
			close(all)
		}
	}, func(err error) {
		mu.Lock()
		gotAtClose = got
		mu.Unlock()
		closed <- err
	}); err != nil {
		_ = unix.Close(wr)
		t.Fatalf("RegisterConn: %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(rd) })

	delivered := false
	select {
	case <-all:
		delivered = true
	case <-time.After(5 * time.Second):
	}
	_ = unix.Close(wr)
	var cerr error
	fired := false
	select {
	case cerr = <-closed:
		fired = true
	case <-time.After(5 * time.Second):
	}
	mu.Lock()
	g, bd, nc, gc := got, bad, calls, gotAtClose
	mu.Unlock()
	t.Logf("C881 integrity: %d of %d bytes in %d onRecv calls (reads of up to %d bytes); first wrong byte at %d; onClose fired %v (%v) with %d bytes delivered",
		g, size, nc, len(w.rbuf), bd, fired, cerr, gc)
	if !delivered {
		t.Errorf("A's onRecv got %d of the %d bytes waiting in its socket within 5 s: the rest was never read", g, size)
	}
	if bd >= 0 {
		t.Errorf("A's bytes arrived out of order: the first wrong byte is at offset %d", bd)
	}
	if !fired {
		t.Errorf("onClose did not fire within 5 s of the write end's close")
	} else if cerr != nil || gc != size {
		t.Errorf("onClose(%v) fired with %d of %d bytes delivered, want onClose(nil) after the last byte", cerr, gc, size)
	}
}

// TestAQueuedConnWaitsForTheNextRound881: a conn that uses up its read budget
// while the worker dispatches a batch gets its next turn only in the next
// round, after the events of the next epoll_wait. A's source is a pipe filled
// with 1 MiB (64 reads, four turns) before A is registered, and nothing more
// arrives. At A's third read, A's onRecv writes B's byte, so B becomes ready
// during A's first turn. B must be served as soon as that turn ends: after
// exactly one turn of A's reads (16 at a budget of 16). A worker that gave A
// its second turn before the next epoll_wait would make B wait two turns
// (32); one that reads A until EAGAIN (the base) makes B wait for all 64.
func TestAQueuedConnWaitsForTheNextRound881(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)

	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		t.Fatalf("pipe2: %v", err)
	}
	rd, wr := p[0], p[1]
	t.Cleanup(func() { _ = unix.Close(rd); _ = unix.Close(wr) })
	const size = 1 << 20
	if _, err := unix.FcntlInt(uintptr(wr), unix.F_SETPIPE_SZ, size); err != nil {
		t.Fatalf("F_SETPIPE_SZ %d: %v", size, err)
	}
	if n, err := unix.Write(wr, make([]byte, size)); n != size || err != nil {
		t.Fatalf("fill the pipe: wrote %d of %d (%v)", n, size, err)
	}
	b, bPeer := socketPair(t)
	t.Cleanup(func() { _ = unix.Close(b); _ = unix.Close(bPeer) })

	var aReads, aReadsAtB atomic.Int64
	bServed := make(chan struct{}, 1)
	if err := w.RegisterConn(b, func([]byte) {
		aReadsAtB.Store(aReads.Load()) // on the worker, the goroutine that counts A's reads
		select {
		case bServed <- struct{}{}:
		default:
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(B): %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(b) })
	if err := w.RegisterConn(rd, func([]byte) {
		if aReads.Add(1) == 3 {
			if _, err := unix.Write(bPeer, []byte{'b'}); err != nil {
				t.Errorf("write B's byte: %v", err)
			}
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}
	t.Cleanup(func() { _ = w.UnregisterConn(rd) })

	select {
	case <-bServed:
	case <-time.After(5 * time.Second):
		t.Fatal("B was not served within 5 s")
	}
	at := aReadsAtB.Load()
	t.Logf("C881 rounds: B became ready at A's read 3 and was served after %d of A's reads", at)
	if at > 16 {
		t.Errorf("B was served after %d of A's reads, want at most one turn (16): A got another turn before the epoll_wait that collected B", at)
	}
}
