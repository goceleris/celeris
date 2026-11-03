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
// pass takes a millisecond more and brings a connection of its own, with its
// request, so that a connection is accepted in every pass and answered in the
// next whatever the machine's speed: a worker that goes on accepting while it
// waits for the head has a response going out at every look, and is held to the
// floor (the regime of a worker with a response to send, which keeps accepting
// for the floor by design, entered through the wait for a head). It is held,
// before the fix, by the look at the connections' unread requests as well.

// holdBound1229 is well under the floor (shutdownSendDrainNanos, 250 ms): a
// Listen that returns inside it was not held by the look.
const holdBound1229 = 60 * time.Millisecond

// held1229 is what the scenario saw.
type held1229 struct {
	resp      *http.Response // the answer to the head that was in the kernel
	respErr   error
	took      time.Duration // from the cancel to Listen's return
	listenErr error
}

// shutdownWithAHeadInTheKernel1229 runs the scenario once. The restores of the
// seam are registered before the engine starts, so the engine's reap, registered
// by start896, runs before them.
func shutdownWithAHeadInTheKernel1229(t *testing.T, churn int) held1229 {
	t.Helper()
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
		if ec, err := net.DialTimeout("tcp", addr.Load().(string), 100*time.Millisecond); err == nil {
			_, _ = ec.Write([]byte(req))
			extraMu.Lock()
			extra = append(extra, ec)
			extraMu.Unlock()
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

	var res held1229
	t0 := time.Now()
	run.cancel()
	res.resp, res.respErr = http.ReadResponse(br, nil)
	returned, err := run.stopped()
	res.took, res.listenErr = returned.Sub(t0), err
	return res
}

// TestShutdownHoldIsNotRenewedByNewConnections1229: Listen must return well
// inside the floor, with the head answered and connections arriving: a worker
// that waits for a head and has nothing to send waits for the heads that were in
// the kernel when it began, and for nothing a later connection brings.
func TestShutdownHoldIsNotRenewedByNewConnections1229(t *testing.T) {
	r := shutdownWithAHeadInTheKernel1229(t, 8)
	if r.listenErr != nil {
		t.Fatalf("Listen returned %v after a cancel", r.listenErr)
	}
	t.Logf("Listen returned %v after the cancel, with connections arriving and a 1 ms drain pass (floor %v)",
		r.took, time.Duration(shutdownSendDrainNanos))
	if r.took > holdBound1229 {
		t.Errorf("Listen took %v to return: new connections renewed the worker's look at the kernel, bound %v, floor %v",
			r.took, holdBound1229, time.Duration(shutdownSendDrainNanos))
	}
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
