//go:build linux

package epoll

// celeris#930: the engine loop's read of a driver conn. driverRead read a
// conn until a read came back shorter than the buffer, with no bound on the
// number of reads, so a conn whose peer kept its socket full held the loop and
// every other conn on it (driver or HTTP) waited. The engine twin of
// celeris#881, which fixed the same loop shape in the standalone driver loop.

import (
	"fmt"
	"net"
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
	// The bound that does not rest on timing: B's byte can arrive in the
	// middle of A's turn, so A reads the rest of that turn, and then the
	// epoll_wait that collects B comes before A's next one.
	if between := aReadsAtB.Load() - readsAtSend; served && between > 2*driverReadBudget {
		t.Errorf("A was read %d times between B's byte and B's service, want at most two turns (%d)", between, 2*driverReadBudget)
	}
}

// TestAQueuedDriverConnWaitsForTheNextRound930 is deterministic. A's source is
// a pipe pre-filled with 1 MiB (32 reads, four turns), and nothing more
// arrives but what A's own onRecv writes. At A's third read, A's onRecv writes
// one more byte into A (a fresh event of A, collected with B's) and then B's
// byte, so the next round's batch is A's event, then B's. B must be served
// after exactly one turn of A's reads (the budget):
//   - A second turn of A in the same round, because A's event in the batch is
//     read as an event of a conn that is not queued, gives twice the budget
//     (the merge is missing).
//   - The base's read until a short read gives all 32.
func TestAQueuedDriverConnWaitsForTheNextRound930(t *testing.T) {
	eng, stop := newTestEngine(t)
	defer stop()
	wl := eng.WorkerLoop(0)

	rd, wr := c930Pipe(t)
	t.Cleanup(func() { _ = unix.Close(rd); _ = unix.Close(wr) })
	const size = 1 << 20
	if n, err := unix.Write(wr, make([]byte, size)); n != size || err != nil {
		t.Fatalf("fill the pipe: wrote %d of %d (%v)", n, size, err)
	}
	b, bPeer := socketpairNonblocking(t)
	t.Cleanup(func() { _ = unix.Close(b); _ = unix.Close(bPeer) })

	var aReads, aReadsAtB atomic.Int64
	bServed := make(chan struct{}, 1)
	if err := wl.RegisterConn(b, func([]byte) {
		aReadsAtB.Store(aReads.Load()) // on the loop, the goroutine that counts A's reads
		select {
		case bServed <- struct{}{}:
		default:
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(B): %v", err)
	}
	t.Cleanup(func() { _ = wl.UnregisterConn(b) })
	if err := wl.RegisterConn(rd, func([]byte) {
		if aReads.Add(1) == 3 {
			if _, err := unix.Write(wr, []byte{'a'}); err != nil {
				t.Errorf("write A's extra byte: %v", err)
			}
			if _, err := unix.Write(bPeer, []byte{'b'}); err != nil {
				t.Errorf("write B's byte: %v", err)
			}
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}
	t.Cleanup(func() { _ = wl.UnregisterConn(rd) })

	select {
	case <-bServed:
	case <-time.After(5 * time.Second):
		t.Fatal("B was not served within 5 s")
	}
	at := aReadsAtB.Load()
	t.Logf("C930 rounds: B became ready at A's read 3 and was served after %d of A's reads (budget %d)", at, driverReadBudget)
	if at != driverReadBudget {
		t.Errorf("B was served after %d of A's reads, want exactly one turn (%d): fewer means the budget is not spent, more means A got another turn before the epoll_wait that collected B", at, driverReadBudget)
	}
}

// TestBudgetedDriverReadsDeliverEveryByteInOrder930: a pipe sized to 1 MiB is
// filled with a counter pattern before it is registered, so the first event
// finds 32 reads' worth (four turns) and nothing new will arrive. Every byte
// must arrive in order, with the loop never waiting in epoll_wait while the
// conn is queued. With the write end closed before the conn is registered, the
// first event carries EPOLLHUP with EPOLLIN, and onClose(nil) must fire after
// the last byte, not at the end of the first turn.
func TestBudgetedDriverReadsDeliverEveryByteInOrder930(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("hangupWithTheFirstEvent=%v", closeFirst), func(t *testing.T) {
			var queuedWaits, blockingWaits atomic.Int64
			testHookDriverQueueWait = func(queued, timeoutMs int) {
				queuedWaits.Add(1)
				if timeoutMs != 0 {
					blockingWaits.Add(1)
				}
			}
			t.Cleanup(func() { testHookDriverQueueWait = nil }) // runs last, after the engine is stopped
			eng, stop := newTestEngine(t)
			t.Cleanup(stop)
			wl := eng.WorkerLoop(0)

			rd, wr := c930Pipe(t)
			t.Cleanup(func() { _ = unix.Close(rd); _ = unix.Close(wr) })
			// 100 bytes short of 32 reads: the last read is short, so the loop does
			// not read on to the EOF, and the teardown the hangup asks for is
			// what closes the conn (a multiple of the buffer would be closed by
			// the zero-byte read after the last full one).
			const size = 1<<20 - 100
			if n, err := unix.Write(wr, c930Pattern(0, size)); n != size || err != nil {
				t.Fatalf("fill the pipe: wrote %d of %d (%v)", n, size, err)
			}
			if closeFirst {
				_ = unix.Close(wr)
				wr = -1
			}
			s := c930NewSink(0, size)
			if err := wl.RegisterConn(rd, s.onRecv, s.onClose); err != nil {
				t.Fatalf("RegisterConn: %v", err)
			}
			t.Cleanup(func() { _ = wl.UnregisterConn(rd) })
			select {
			case <-s.all:
			case <-time.After(5 * time.Second):
				s.mu.Lock()
				got := s.got
				s.mu.Unlock()
				t.Fatalf("A got %d of %d bytes within 5 s", got, size)
			}
			if !closeFirst {
				_ = unix.Close(wr)
				wr = -1
			}
			select {
			case err := <-s.closed:
				if err != nil {
					t.Errorf("onClose(%v), want nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("onClose never fired after the write end was closed")
			}
			s.mu.Lock()
			bad, atClose, closes, reads := s.bad, s.atClose, s.closes, s.reads
			s.mu.Unlock()
			q, bw := queuedWaits.Load(), blockingWaits.Load()
			t.Logf("C930 delivery: %d reads, first byte out of order at %d, %d bytes at onClose (%d onClose calls); epoll_waits with a conn queued: %d, with a nonzero timeout: %d",
				reads, bad, atClose, closes, q, bw)
			if bad >= 0 {
				t.Errorf("byte %d arrived out of order", bad)
			}
			if atClose != size || closes != 1 {
				t.Errorf("onClose fired %d times, with %d of %d bytes delivered at the first: the teardown did not wait for the drain", closes, atClose, size)
			}
			if q < 3 {
				t.Errorf("the loop made %d epoll_waits with a conn queued, want at least 3 (four turns): the test did not queue the conn", q)
			}
			if bw != 0 {
				t.Errorf("the loop made %d of its %d epoll_waits with a conn queued with a nonzero timeout: edge-triggered epoll will not report the bytes it is owed", bw, q)
			}
		})
	}
}

// TestAFloodedDriverConnGetsEveryByteBeforeItsPeersFIN930: A's peer writes
// 4 MiB as fast as A's socket takes it, then closes. The close reaches A as
// EPOLLRDHUP/EPOLLHUP with the last of the flood still in the socket, on a
// turn that stops at its budget. onClose must fire after the last byte.
func TestAFloodedDriverConnGetsEveryByteBeforeItsPeersFIN930(t *testing.T) {
	eng, stop := newTestEngine(t)
	defer stop()
	wl := eng.WorkerLoop(0)
	a, aPeer := socketpairNonblocking(t)
	t.Cleanup(func() { _ = unix.Close(a) })
	const total = 4<<20 - 100 // not a multiple of the read buffer: see TestBudgetedDriverReadsDeliverEveryByteInOrder930
	s := c930NewSink(0, total)
	if err := wl.RegisterConn(a, func(b []byte) {
		s.onRecv(b)
		time.Sleep(20 * time.Microsecond) // slower than the feeder: the socket stays non-empty
	}, s.onClose); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	t.Cleanup(func() { _ = wl.UnregisterConn(a) })
	pat := c930Pattern(0, total)
	go func() {
		for off := 0; off < total; {
			chunk := pat[off:]
			if len(chunk) > 64<<10 {
				chunk = chunk[:64<<10]
			}
			n, err := unix.Write(aPeer, chunk)
			if n > 0 {
				off += n
			}
			if err == unix.EAGAIN {
				time.Sleep(10 * time.Microsecond)
				continue
			}
			if err != nil {
				return
			}
		}
		_ = unix.Close(aPeer)
	}()
	select {
	case <-s.closed:
	case <-time.After(30 * time.Second):
		t.Fatal("onClose never fired")
	}
	s.mu.Lock()
	got, bad, closes := s.got, s.bad, s.closes
	s.mu.Unlock()
	t.Logf("C930 FIN: %d of %d bytes at onClose (%d calls), first byte out of order at %d", got, total, closes, bad)
	if got != total || bad >= 0 {
		t.Errorf("onClose fired with %d of %d bytes delivered (first byte out of order: %d)", got, total, bad)
	}
}

// TestAQueuedDriverConnUnregisteredBeforeItsTurnReadsNothing930: A is queued
// after a turn. The test unregisters A from the loop's goroutine, inside the
// epoll_wait hook that precedes the conn's next turn. A's onClose fires once
// and A's onRecv never runs again: the turn finds the conn closed (the
// closed check of celeris#710 covers the queue).
func TestAQueuedDriverConnUnregisteredBeforeItsTurnReadsNothing930(t *testing.T) {
	eng, stop := newTestEngine(t)
	wl := eng.WorkerLoop(0)
	rd, wr := c930Pipe(t)
	t.Cleanup(func() { _ = unix.Close(rd); _ = unix.Close(wr) })
	if n, err := unix.Write(wr, make([]byte, 1<<20)); n != 1<<20 || err != nil {
		t.Fatalf("fill the pipe: wrote %d (%v)", n, err)
	}
	var reads, readsAtUnregister, closes atomic.Int64
	var once sync.Once
	unregistered := make(chan struct{})
	testHookDriverQueueWait = func(queued, timeoutMs int) {
		once.Do(func() {
			readsAtUnregister.Store(reads.Load())
			_ = wl.UnregisterConn(rd)
			close(unregistered)
		})
	}
	t.Cleanup(func() { testHookDriverQueueWait = nil }) // after stop
	t.Cleanup(stop)
	if err := wl.RegisterConn(rd, func([]byte) { reads.Add(1) }, func(error) { closes.Add(1) }); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	select {
	case <-unregistered:
	case <-time.After(5 * time.Second):
		t.Fatal("A was never queued")
	}
	time.Sleep(50 * time.Millisecond) // the turns A would have been owed
	t.Logf("C930 unregister: A read %d chunks before the unregister and %d after; onClose calls: %d", readsAtUnregister.Load(), reads.Load()-readsAtUnregister.Load(), closes.Load())
	if readsAtUnregister.Load() != driverReadBudget {
		t.Errorf("A had %d reads when it was unregistered, want one turn (%d)", readsAtUnregister.Load(), driverReadBudget)
	}
	if after := reads.Load() - readsAtUnregister.Load(); after != 0 {
		t.Errorf("A's onRecv ran %d times after UnregisterConn returned", after)
	}
	if closes.Load() != 1 {
		t.Errorf("A's onClose fired %d times, want 1", closes.Load())
	}
}

// TestAnHTTPConnOnALoopWithAFloodingDriverConnIsServed930: both loops of an
// engine carry a flooding driver conn, so an HTTP conn is on a flooded loop
// whichever one the kernel gives it. Each HTTP request must be answered within
// 3 s while the floods go on; the base never answers a request on a flooded
// loop while the flood lasts.
func TestAnHTTPConnOnALoopWithAFloodingDriverConnIsServed930(t *testing.T) {
	e := c930HTTPEngine(t)
	var floods []*c930Flooder
	for i := 0; i < e.NumWorkers(); i++ {
		f := c930Flood(t, e.WorkerLoop(i), 100*time.Microsecond)
		floods = append(floods, f)
		t.Cleanup(f.close)
	}
	c, err := net.DialTimeout("tcp", e.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	time.Sleep(20 * time.Millisecond)
	var worst time.Duration
	buf := make([]byte, 512)
	for i := 0; i < 20; i++ {
		start := time.Now()
		_ = c.SetDeadline(start.Add(3 * time.Second))
		if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("request %d: write: %v", i, err)
		}
		got := 0
		for got < 4 || string(buf[got-2:got]) != "ok" {
			n, err := c.Read(buf[got:])
			if err != nil {
				t.Errorf("request %d: no response within 3 s while the driver floods went on (floods fed %d and %d bytes): %v",
					i, floods[0].fed.Load(), floods[1].fed.Load(), err)
				return
			}
			got += n
		}
		if d := time.Since(start); d > worst {
			worst = d
		}
	}
	t.Logf("C930 HTTP beside the floods: 20 requests answered, slowest %v; floods delivered %d and %d bytes", worst, floods[0].got.Load(), floods[1].got.Load())
}

// TestAFloodedDriverConnWithEPOLLOUTArmedKeepsItsFlushAndOneTurnPerRound930:
// flushDriverSendLocked arms EPOLLOUT with a MOD that drops EPOLLET, and the
// flag belongs to the whole registration, so while EPOLLOUT is armed the conn's
// EPOLLIN is level-triggered and every round reports a flooded conn again. A
// floods, and A's owner writes 8 MiB into A, whose peer does not read, so the
// write hits EAGAIN and EPOLLOUT is armed. B must still be served within two
// turns of A's reads, and when A's peer reads the 8 MiB at last the flush must
// run to the end and EPOLLOUT disarm: the merge of a queued conn's event keeps
// its EPOLLOUT.
func TestAFloodedDriverConnWithEPOLLOUTArmedKeepsItsFlushAndOneTurnPerRound930(t *testing.T) {
	eng, stop := newTestEngine(t)
	t.Cleanup(stop) // runs after f.close below: the engine cannot stop while a flood holds its loop
	wl := eng.WorkerLoop(0)
	var aReads atomic.Int64
	f := c930FloodCounting(t, wl, 100*time.Microsecond, &aReads)
	t.Cleanup(f.close)
	bfd, bPeer := socketpairNonblocking(t)
	t.Cleanup(func() { _ = wl.UnregisterConn(bfd); _ = unix.Close(bfd); _ = unix.Close(bPeer) })
	var aReadsAtB atomic.Int64
	bGot := make(chan struct{}, 1)
	if err := wl.RegisterConn(bfd, func([]byte) {
		aReadsAtB.Store(aReads.Load())
		select {
		case bGot <- struct{}{}:
		default:
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(B): %v", err)
	}

	const out = 8 << 20
	if err := wl.Write(f.fd, make([]byte, out)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	dc := wl.(*Loop).lookupDriver(f.fd)
	armed := false
	for dl := time.Now().Add(2 * time.Second); !armed && time.Now().Before(dl); time.Sleep(time.Millisecond) {
		dc.mu.Lock()
		armed = dc.epollOut
		dc.mu.Unlock()
	}
	if !armed {
		t.Fatal("EPOLLOUT was not armed after an 8 MiB write into a socket nobody reads")
	}
	readsAtSend := aReads.Load()
	sent := time.Now()
	if _, err := unix.Write(bPeer, []byte{'b'}); err != nil {
		t.Fatalf("write B: %v", err)
	}
	select {
	case <-bGot:
	case <-time.After(3 * time.Second):
		t.Fatal("B was not served within 3 s while A flooded with EPOLLOUT armed")
	}
	waited, between := time.Since(sent), aReadsAtB.Load()-readsAtSend

	// A's peer reads A's 8 MiB: the flush resumes on EPOLLOUT and finishes.
	got := 0
	buf := make([]byte, 256<<10)
	for dl := time.Now().Add(10 * time.Second); got < out && time.Now().Before(dl); {
		n, err := unix.Read(f.peer, buf)
		if n > 0 {
			got += n
			continue
		}
		if err != nil && err != unix.EAGAIN {
			t.Fatalf("read A's peer: %v", err)
		}
		time.Sleep(200 * time.Microsecond)
	}
	disarmed := false
	for dl := time.Now().Add(2 * time.Second); !disarmed && time.Now().Before(dl); time.Sleep(time.Millisecond) {
		dc.mu.Lock()
		disarmed = !dc.epollOut
		dc.mu.Unlock()
	}
	t.Logf("C930 EPOLLOUT armed: B served %v after its byte with %d of A's reads in between; A's peer read %d of %d bytes; EPOLLOUT disarmed: %v", waited, between, got, out, disarmed)
	if between > 2*driverReadBudget {
		t.Errorf("A was read %d times between B's byte and B's service, want at most two turns (%d)", between, 2*driverReadBudget)
	}
	if got != out || !disarmed {
		t.Errorf("A's peer read %d of %d bytes and EPOLLOUT disarmed: %v: the flush did not run to the end under the flood", got, out, disarmed)
	}
}
