//go:build linux

package iouring

import (
	"bufio"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/drainclock"
)

// celeris#1229. A worker with nothing to send waits, inside the 250 ms floor of
// its send drain, for a request already in the kernel (celeris#1208). That look
// used to be made again on every pass, with the listener still open, so a
// connection accepted after the shutdown began, with its request, renewed the
// hold: under steady arrivals (the churn clients of the listen-release tests) a
// worker held to the floor, 400 shutdowns of them in the Coverage job, where the
// instrumented worker is slower than its clients. The look is now taken once, at
// the first pass of the drain, and the worker waits for the conns it marked.
//
// The scenario below is the same for both tests: a keep-alive conn the worker
// has accepted gets its next request while the workers are in their first drain
// pass, before their snapshot (a head in the kernel when the worker sees the
// shutdown, whatever the machine's speed), and 8 clients, started at that
// moment, whose connections and requests wait in the kernel's accept queue and
// go on arriving, as the churn of the listen-release tests does. Every drain
// pass takes a millisecond more and brings perPass1229 connections of its own,
// with a request each, so that a connection is accepted in every pass and
// answered in the next whatever the machine's speed: a worker that goes on
// accepting while it waits for the head has a response going out at every look,
// and is held to the floor (the regime of a worker with a response to send,
// which keeps accepting for the floor by design, entered through the wait for a
// head). It is held, before the fix, by the look at the connections' unread
// requests as well.

// The tests below do not time Listen. A bound on the time it takes, set well
// under the floor (shutdownSendDrainNanos, 250 ms), failed on a machine that
// starved the test, not the engine: 6 runs of 200 at 8 busy-loop CPU hogs on 2
// CPUs took 184 to 301 ms against 150, and the worker's own part of that, the
// time outside the test's seam, was 4 ms at the most (celeris#1258). The seam
// sleeps and dials on the worker's thread, and a thread that the hogs starve
// takes 100 to 200 ms for the 25 ms of it. They hold the drain's clock instead
// (drainclock.SetSeam, as celeris#1244's cells do), at the instant of the cancel,
// so that the floor cannot run out on its own, and judge whether Listen returns:
// a worker that the look at the kernel, or the open listener, holds is held to a
// floor that does not come, and Listen does not return until the test thaws the
// clock in its cleanup; one that is not held returns whatever the machine's
// speed. The tests wait listenWait1229 for it, which is no bound on the engine's
// speed either: a longer wait changes no verdict.
const listenWait1229 = 20 * time.Second

// perPass1229 is how many connections, with a request each, every drain pass
// brings, and keepExtra1229 how many of them the test keeps open (the oldest is
// closed when there are more: a worker held for the wait of a test sees tens of
// thousands of passes). A worker that goes on accepting is held by a response
// going out in every pass, which needs a connection in the accept queue in every
// pass: one connection a pass, and the 8 clients behind it, left the queue empty
// in a pass now and then when the machine starved them (7 runs of 80 at 8 CPU
// hogs on 2 CPUs, for the engine that keeps its listener open while only the
// look is outstanding, celeris#1258); several a pass leave a backlog.
const (
	perPass1229   = 4
	keepExtra1229 = 256
)

// holdDrainClock1229 holds the drain's clock for the test: the real monotonic
// clock until freeze, and then the instant it was frozen at. The seam is
// restored after the engine's stop (the test registers the thaw after it starts
// the engine, so that it runs before).
func holdDrainClock1229(t *testing.T) *heldClock1244 {
	t.Helper()
	clk := &heldClock1244{epoch: time.Now()}
	t.Cleanup(drainclock.SetSeam(clk.read))
	return clk
}

// listenReturned1229 waits for Listen, cancelled at t0 and with the drain's
// clock held, for up to listenWait1229, and says whether it returned, when (by
// the test's clock, which is logged and never judged) and with what error.
func listenReturned1229(run *engineRun896, t0 time.Time) (returned bool, took time.Duration, err error) {
	select {
	case err = <-run.done:
		return true, time.Since(t0), err
	case <-time.After(listenWait1229):
		return false, 0, nil
	}
}

// held1229 is what the scenario saw.
type held1229 struct {
	resp    *http.Response // the answer to the head that was in the kernel
	respErr error
	run     *engineRun896
	t0      time.Time // the cancel
}

// shutdownWithAHeadInTheKernel1229 runs the scenario once, up to the answer to
// the head, with the drain's clock held from the cancel on. The restores of the
// seams are registered before the engine starts, so the engine's reap,
// registered by start896, runs before them, and the thaw after the start, so
// that it runs before the reap: a worker held by the floor (the engine these
// tests are red on) goes on when the clock does, and the engine stops.
func shutdownWithAHeadInTheKernel1229(t *testing.T, churn int) *held1229 {
	t.Helper()
	clk := holdDrainClock1229(t)
	var (
		mu       sync.Mutex
		entered  = map[*Worker]bool{}
		wrote    sync.Once
		nWorkers atomic.Int64 // set once the engine has started
		addr     atomic.Value // string, as nWorkers
		write    atomic.Pointer[func()]
		extra    []net.Conn // the connections the passes bring; the seam runs on the worker's thread, a pass at a time per worker
		extraMu  sync.Mutex
		stop     = make(chan struct{})
		churnWG  atomic.Pointer[sync.WaitGroup]
	)
	const req = "GET /x HTTP/1.1\r\nHost: x\r\n\r\n"
	hold := func(w *Worker) {
		mu.Lock()
		entered[w] = true
		n := len(entered)
		mu.Unlock()
		// Every worker waits for all of them to be in the drain, so the head
		// is written before any has taken its snapshot.
		for dl := time.Now().Add(5 * time.Second); int64(n) < nWorkers.Load() && time.Now().Before(dl); {
			time.Sleep(100 * time.Microsecond)
			mu.Lock()
			n = len(entered)
			mu.Unlock()
		}
		if f := write.Load(); f != nil {
			wrote.Do(func() {
				(*f)()
				// The clients begin once the workers are held: their
				// connections are in the kernel's accept queue, with their
				// requests, when the first drain pass runs.
				churnWG.Store(churnN896(addr.Load().(string), stop, churn))
				time.Sleep(20 * time.Millisecond)
			})
		}
		for range perPass1229 {
			if ec, err := net.DialTimeout("tcp", addr.Load().(string), 100*time.Millisecond); err == nil {
				_, _ = ec.Write([]byte(req))
				extraMu.Lock()
				extra = append(extra, ec)
				if len(extra) > keepExtra1229 {
					_ = extra[0].Close()
					extra = extra[1:]
				}
				extraMu.Unlock()
			}
		}
		time.Sleep(time.Millisecond)
	}
	sendDrainSeam.Store(&hold)
	t.Cleanup(func() {
		sendDrainSeam.Store(nil)
		extraMu.Lock()
		defer extraMu.Unlock()
		for _, ec := range extra {
			_ = ec.Close()
		}
	})

	a := freeAddr639(t)
	addr.Store(a)
	run := start896(t, a, 2)
	nWorkers.Store(int64(len(run.e.workers)))
	t.Logf("engine: %d workers, io_uring tier %s", len(run.e.workers), tier844(run.e))
	t.Cleanup(clk.thaw)

	c, err := net.DialTimeout("tcp", a, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatalf("write: %v", err)
	}
	first, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read the first response: %v", err)
	}
	_, _ = br.Discard(int(first.ContentLength))
	if first.StatusCode != 200 {
		t.Fatalf("first request: status %d", first.StatusCode)
	}

	t.Cleanup(func() {
		close(stop)
		if wg := churnWG.Load(); wg != nil {
			wg.Wait()
		}
	})

	// Written by the first worker to be in its drain, once all are.
	w := func() { _, _ = c.Write([]byte(req)) }
	write.Store(&w)

	// The answer is awaited for as long as the wait for Listen: it does not
	// depend on the floor either, and a deadline of the first request, set
	// before the engine's start and the machine's stalls, must not be the end of
	// it.
	_ = c.SetDeadline(time.Now().Add(listenWait1229))
	res := &held1229{run: run}
	clk.freeze()
	res.t0 = time.Now()
	run.cancel()
	res.resp, res.respErr = http.ReadResponse(br, nil)
	return res
}

// TestShutdownHoldIsNotRenewedByNewConnections1229: Listen must return, with
// the head answered and connections arriving, while the drain's clock is held
// and the floor cannot run out. A worker that waits for a head and has nothing
// to send waits for the heads that were in the kernel when it began, and for
// nothing a later connection brings; one that the look at the kernel, renewed by
// every connection a pass accepts, or the listener, which brings a response to
// send, holds, is held to a floor that does not come, and Listen does not return
// (the engine before celeris#1243, and one that keeps the listener open while
// only the look is outstanding).
func TestShutdownHoldIsNotRenewedByNewConnections1229(t *testing.T) {
	r := shutdownWithAHeadInTheKernel1229(t, 8)
	returned, took, err := listenReturned1229(r.run, r.t0)
	if !returned {
		t.Fatalf("Listen had not returned %v after the cancel with the drain's clock held (floor %v, which cannot run out): "+
			"new connections renewed the worker's look at the kernel, or the open listener brought it responses to send",
			listenWait1229, time.Duration(shutdownSendDrainNanos))
	}
	if err != nil {
		t.Fatalf("Listen returned %v after a cancel", err)
	}
	t.Logf("Listen returned %v after the cancel, by the test's clock, with the drain's clock held (floor %v)",
		took, time.Duration(shutdownSendDrainNanos))
}

// TestShutdownAnswersAHeadInTheKernelUnderChurn1229 is the celeris#1208
// contract under the same churn: the head that was in the kernel when the worker
// saw the shutdown is answered, with Connection: close.
func TestShutdownAnswersAHeadInTheKernelUnderChurn1229(t *testing.T) {
	r := shutdownWithAHeadInTheKernel1229(t, 8)
	if r.respErr != nil {
		t.Fatalf("the head that was in the kernel when the shutdown began got no answer: %v", r.respErr)
	}
	// net/http takes the Connection: close header out of the map and sets Close.
	if r.resp.StatusCode != 200 || !r.resp.Close {
		t.Errorf("answer to the head: status %d, Connection: close %v, want 200 and close", r.resp.StatusCode, r.resp.Close)
	}
}

// TestShutdownSnapshotIsTakenOnce1229 pins that the look at the kernel is a
// snapshot of the first pass of the drain, and not repeated: two keep-alive
// conns of the SAME worker, the first gets its next request while the workers
// are held in their first drain pass (before the snapshot), the second gets it
// only in the pass in which that worker has nothing left to wait for (nothing to
// send, no request in progress, and the look at the kernel clear: the first
// conn's request read), so that the worker is done in that very pass, just
// after the seam. The first is answered, with Connection: close; the second is
// not waited for, and its request is dropped as that of any connection that
// sends after the worker has looked (a snapshot taken on every pass would mark
// it and answer it).
//
// Two things the scenario must not leave to the machine (celeris#1254):
//
//   - The second head is not written while the first is still unread. Nothing to
//     send and no request in progress is also what the worker looks like in the
//     pass after the first drain pass when that pass's reap did not take the
//     first conn's recv completion (it is not in the CQ ring yet, or the recv
//     was not armed): the worker is then held by the first head, and the
//     ordinary path reads the second conn's head in the same reap as the first
//     and answers it, as it answers any request in progress, whatever the
//     snapshot.
//   - The two conns are on one worker. The runner of the base tier has memlock
//     for several workers, and a head written to a conn of another worker, one
//     that has not exited yet (it has a send to reap) and that is not held, is
//     read by it and answered. Each worker says, in its first pass, which of the
//     test's conns it has accepted (by their address); the conns are one more
//     than the workers, so that some worker has two; the first of them to have
//     two is the one the heads go to.
//
// The test fails, and does not pass for want of a look, when no worker had two
// conns or the second head was never written.
func TestShutdownSnapshotIsTakenOnce1229(t *testing.T) {
	const req = "GET /x HTTP/1.1\r\nHost: x\r\n\r\n"
	var (
		mu     sync.Mutex
		calls  = map[*Worker]int{}
		owned  = map[*Worker][]int{} // the indexes of the test's conns each worker has accepted
		nW     atomic.Int64
		first  sync.Once
		second sync.Once
		wrote2 atomic.Bool
		star   atomic.Pointer[Worker] // the worker both heads go to
		chosen = make(chan struct{})  // closed once star is chosen (or found to be none)
		byAddr = map[string]int{}     // the client address of each conn, to its index; set before the cancel
		cs     []net.Conn
		i1, i2 int // the conns of the heads, set before chosen is closed
	)
	hold := func(w *Worker) {
		mu.Lock()
		calls[w]++
		n, entered := calls[w], len(calls)
		if n == 1 {
			mine := []int{}
			for _, fd := range w.liveConns {
				if c := w.conns[fd]; c != nil {
					if i, ok := byAddr[c.remoteAddr]; ok {
						mine = append(mine, i)
					}
				}
			}
			owned[w] = mine
		}
		mu.Unlock()
		if n == 1 {
			// Every worker waits for all of them to be in the drain, with its
			// conns told, so that the head is written before any has taken its
			// snapshot.
			for dl := time.Now().Add(5 * time.Second); int64(entered) < nW.Load() && time.Now().Before(dl); {
				time.Sleep(100 * time.Microsecond)
				mu.Lock()
				entered = len(calls)
				mu.Unlock()
			}
			first.Do(func() {
				defer close(chosen)
				mu.Lock()
				var best *Worker
				for ww, o := range owned {
					if len(o) >= 2 && (best == nil || len(o) > len(owned[best]) || len(o) == len(owned[best]) && ww.id < best.id) {
						best = ww
					}
				}
				if best != nil {
					i1, i2 = owned[best][0], owned[best][1]
				}
				mu.Unlock()
				if best == nil {
					return
				}
				star.Store(best)
				_, _ = cs[i1].Write([]byte(req))
			})
			return
		}
		// In the pass in which the worker is done unless something new marks a
		// conn: no send, no request in progress, and nothing of the first
		// conn's head left in the kernel (the look the snapshot of the first
		// pass left).
		if w == star.Load() && !w.hasPendingSends() && !w.h1RequestInProgress() && !w.h1UnreadInKernel() {
			second.Do(func() {
				wrote2.Store(true)
				_, _ = cs[i2].Write([]byte(req))
			})
		}
	}
	clk := holdDrainClock1229(t)
	sendDrainSeam.Store(&hold)
	t.Cleanup(func() { sendDrainSeam.Store(nil) })

	addr := freeAddr639(t)
	run := start896(t, addr, 2)
	nW.Store(int64(len(run.e.workers)))
	t.Cleanup(clk.thaw)

	brs := make([]*bufio.Reader, nW.Load()+1)
	for i := range brs {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		brs[i] = bufio.NewReader(c)
		if _, err := c.Write([]byte(req)); err != nil {
			t.Fatalf("write: %v", err)
		}
		resp, err := http.ReadResponse(brs[i], nil)
		if err != nil {
			t.Fatalf("read the first response: %v", err)
		}
		_, _ = brs[i].Discard(int(resp.ContentLength))
		cs = append(cs, c)
		byAddr[c.LocalAddr().String()] = i
	}
	for _, c := range cs {
		_ = c.SetDeadline(time.Now().Add(listenWait1229))
	}

	clk.freeze()
	t0 := time.Now()
	run.cancel()
	select {
	case <-chosen:
	case <-time.After(5 * time.Second):
		t.Fatal("premise: no worker reached its drain")
	}
	if star.Load() == nil {
		t.Fatalf("premise: no worker had accepted two of the %d conns (%d workers)", len(cs), nW.Load())
	}
	resp1, err1 := http.ReadResponse(brs[i1], nil)
	resp2, err2 := http.ReadResponse(brs[i2], nil)
	returned, took, err := listenReturned1229(run, t0)
	if !returned {
		t.Errorf("Listen had not returned %v after the cancel with the drain's clock held (floor %v, which cannot run out)",
			listenWait1229, time.Duration(shutdownSendDrainNanos))
	} else if err != nil {
		t.Fatalf("Listen returned %v after a cancel", err)
	}
	if err1 != nil {
		t.Fatalf("the head that was in the kernel when the shutdown began got no answer: %v", err1)
	}
	if !wrote2.Load() {
		t.Fatal("premise: the second head was never written (no pass after the first head's was quiet), so an unanswered second conn says nothing of the snapshot")
	}
	if resp1.StatusCode != 200 || !resp1.Close {
		t.Errorf("answer to the first head: status %d, Connection: close %v, want 200 and close", resp1.StatusCode, resp1.Close)
	}
	if err2 == nil {
		t.Errorf("the head that reached the second conn after the snapshot was answered (status %d): the look is made again after the first pass", resp2.StatusCode)
	}
	if returned {
		t.Logf("Listen returned %v after the cancel, by the test's clock, with the drain's clock held (floor %v)",
			took, time.Duration(shutdownSendDrainNanos))
	}
}
