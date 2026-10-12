package postgres

// celeris#887 item 20 (the release side, found as the CI flake of
// TestPgNetConnServerHangUpFailsLaterQueries): failAll takes the pending queue
// and then closes each request's channels and fails it, on the loop goroutine.
// The caller whose enqueue raced the close fails its own request through
// failReq (which no longer finds it in pending, so it only calls fail) and
// releases it: releasePgRequest resets the request's fields and puts it back in
// the pool while failAll still reads and writes them. The race detector reports
// it as releasePgRequest against closeRowCh; without it, the same window lets
// failAll close the channels of a request the pool already handed to the next
// query.
//
// The window is forced here without a hook in the driver: the test holds the
// request's chMu, which failAll takes (closeRowCh) as its first access to a
// request after it took the queue, runs the caller's half in that gap, and only
// then lets failAll go on. A forced order is also a synchronised one, so the
// race detector cannot see this order; the tests assert what a recycled
// request looks like instead (the -race run of TestPgNetConnServerHangUp is the
// detector's witness).

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
)

// c887Gated queues req on a fresh conn, holds gate (a mutex of req that
// failAll takes), starts failAll on its own goroutine and returns once failAll
// has taken the queue (so the caller's failReq can no longer find req in
// pending). failAll then waits at gate until the returned release func runs;
// wait blocks until failAll has returned. With gate = &req.chMu that is
// failAll's first access to req (closeRowCh); with &req.doneMu it is its last
// (fail).
func c887Gated(t *testing.T, req *pgRequest, err error, gate sync.Locker) (c *pgConn, release func(), wait func()) {
	t.Helper()
	c = &pgConn{bridge: async.NewBridge()}
	c.enqueue(req)
	gate.Lock()
	done := make(chan struct{})
	go func() { c.failAll(err); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.pendingMu.Lock()
		taken := len(c.pending) == 0
		c.pendingMu.Unlock()
		if taken {
			break
		}
		if time.Now().After(deadline) {
			gate.Unlock()
			t.Fatal("failAll never took the pending queue")
		}
		time.Sleep(time.Millisecond)
	}
	unlocked := false
	release = func() {
		if !unlocked {
			unlocked = true
			gate.Unlock()
		}
	}
	t.Cleanup(release)
	wait = func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("failAll did not return within 5 s")
		}
	}
	return c, release, wait
}

// c887PooledRequest is what simpleExec holds: a pooled request, ctx set, queued.
func c887PooledRequest(t *testing.T) *pgRequest {
	t.Helper()
	req := acquirePgRequest()
	if !req.fromPool {
		t.Fatal("acquirePgRequest returned a request that is not pooled")
	}
	req.ctx = context.WithValue(context.Background(), c887Key{}, "owned by the caller")
	req.kind = reqSimple
	return req
}

type c887Key struct{}

// The caller releases the request after failAll took it off the queue and
// before failAll touched it. The request must not be recycled under failAll:
// its fields (and its place outside the pool) stay as they were.
func TestReleaseWhileFailAllHoldsTheRequestDoesNotRecycleIt887(t *testing.T) {
	req := c887PooledRequest(t)
	boom := errors.New("conn closed")
	c, release, wait := c887Gated(t, req, boom, &req.chMu)

	// The caller's half: its Write found the conn closed.
	callerErr := errors.New("write on a closed conn")
	c.failReq(req, callerErr)
	if !req.finished.Load() {
		t.Fatal("failReq did not finish the request: the scenario is not the caller-releases-early one")
	}
	releasePgRequest(req)
	if req.ctx == nil {
		t.Error("releasePgRequest reset a request that failAll still holds: its fields are recycled under the sweep")
	}

	release()
	wait()
	if !errors.Is(req.err, callerErr) {
		t.Errorf("req.err = %v, want the caller's own failure (first fail wins)", req.err)
	}
	if ep := req.streamErr.Load(); ep == nil || !errors.Is(*ep, boom) {
		t.Errorf("streamErr = %v, want the failAll error stored on the request it swept", ep)
	}
}

// The same window on a streaming request: failAll owes its consumer a closed
// rowCh and colsCh, and releasePgRequest nils both (and clears the closing
// flags), so a failAll that runs after the release closes nothing and the
// consumer parks for good.
func TestReleaseWhileFailAllHoldsAStreamingRequestStillClosesItsChannels887(t *testing.T) {
	req := c887PooledRequest(t)
	req.rowCh = make(chan [][]byte, 2)
	req.colsCh = make(chan struct{})
	rowCh, colsCh := req.rowCh, req.colsCh
	boom := errors.New("conn closed")
	c, release, wait := c887Gated(t, req, boom, &req.chMu)

	c.failReq(req, errors.New("write on a closed conn"))
	releasePgRequest(req)

	release()
	wait()
	select {
	case _, ok := <-rowCh:
		if ok {
			t.Error("rowCh delivered a row, want it closed")
		}
	default:
		t.Error("rowCh is still open after failAll: the release reset the request before failAll could close it")
	}
	select {
	case <-colsCh:
	default:
		t.Error("colsCh is still open after failAll: the consumer parked on it is never woken")
	}
}

// The control that keeps the fix from becoming "never pool": a request no
// failAll holds any more is recycled by releasePgRequest as before, and one
// that failAll swept and finished with is too.
func TestReleaseAfterFailAllFinishedRecyclesTheRequest887(t *testing.T) {
	req := c887PooledRequest(t)
	c := &pgConn{bridge: async.NewBridge()}
	c.enqueue(req)
	c.failAll(errors.New("conn closed"))
	releasePgRequest(req)
	if req.ctx != nil {
		t.Error("a request nothing holds was not recycled: the pin outlived failAll")
	}

	req2 := c887PooledRequest(t)
	req2.fail(errors.New("failed by its caller"))
	releasePgRequest(req2)
	if req2.ctx != nil {
		t.Error("a finished, unswept request was not recycled")
	}
}

// The other end of the sweep: failAll has closed what it had to and is about
// to fail the request (blocked on doneMu). A release in that gap would recycle
// a request failAll is still going to fail, which can be the next query's by
// then, so the pin must outlive everything up to and including fail().
func TestReleaseWhileFailAllIsAboutToFailTheRequestDoesNotRecycleIt887(t *testing.T) {
	req := c887PooledRequest(t)
	c, release, wait := c887Gated(t, req, errors.New("conn closed"), &req.doneMu)
	_ = c

	// Give failAll time to run up to its fail() and park on doneMu (it has no
	// observable arrival). The caller's half: the request is finished some
	// other way and the caller releases it.
	time.Sleep(100 * time.Millisecond)
	req.finished.Store(true)
	releasePgRequest(req)
	if req.ctx == nil {
		t.Error("releasePgRequest recycled a request failAll was about to fail: the pin ended before fail()")
	}
	release()
	wait()
}
