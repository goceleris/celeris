package postgres

// celeris#887 items 22, 23 and 26 (driver/postgres). All three were found by
// code reading and never run; each test here is a probe that asserts the
// property the item says is broken.
//
//   - 22: a panic (or Goexit) that leaves WriteAndPoll must not leave writerMu
//     locked and the conn usable. The probe injects the panic through the
//     syncLoop interface field (a fake round tripper), the same seam the real
//     eventloop worker is installed through.
//   - 23: failAll on another goroutine wakes the caller (through doneCh) while
//     dispatch is still appending a DataRow to req.rows, and the caller reads
//     req.rows. The detector reports it; the test needs -race to fail (a torn
//     slice header is not observable otherwise).
//   - 26: dispatch peeks the head request under pendingMu and then works on it
//     with no pin; a failAll that runs in that gap lets the caller release (and
//     the pool recycle) a request dispatch still completes.

import (
	"context"
	"encoding/binary"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// panicSync is a syncRoundTripper whose WriteAndPoll panics, or ends the
// goroutine, the way a panicking or Goexit-ing onRecv callback leaves the real
// worker's WriteAndPoll (whose deferred releaseOnExit re-arms the fd and lets
// the panic propagate).
type panicSync struct{ goexit bool }

func (p panicSync) WriteAndPoll(int, []byte, []byte, func([]byte)) (bool, error) {
	if p.goexit {
		runtime.Goexit()
	}
	panic("an onRecv callback panicked inside WriteAndPoll")
}

func newSyncFaultConn(s syncRoundTripper) *pgConn {
	c := &pgConn{
		bridge:   async.NewBridge(),
		writer:   protocol.NewWriter(),
		reader:   protocol.NewReader(),
		syncLoop: &pgSyncLoop{sync: s},
		syncBuf:  make([]byte, 1024),
	}
	c.onRecvFn = c.onRecv
	return c
}

// syncCalls are the five call sites that hold writerMu across WriteAndPoll.
var syncCalls = []struct {
	name string
	run  func(c *pgConn, ctx context.Context)
}{
	{"simpleQuery", func(c *pgConn, ctx context.Context) { _, _ = c.simpleQuery(ctx, "SELECT 1") }},
	{"simpleExec", func(c *pgConn, ctx context.Context) { _, _, _ = c.simpleExec(ctx, "SELECT 1") }},
	{"simpleExecNoTag", func(c *pgConn, ctx context.Context) { _ = c.simpleExecNoTag(ctx, "SELECT 1") }},
	{"doExtendedQuery", func(c *pgConn, ctx context.Context) { _, _ = c.doExtendedQuery(ctx, "", "SELECT 1", nil, nil) }},
	{"doExtendedExec", func(c *pgConn, ctx context.Context) { _, _ = c.doExtendedExec(ctx, "", "SELECT 1", nil) }},
}

// A panic that leaves WriteAndPoll and is recovered by the caller (an HTTP
// recover middleware around a handler that issued the query; Pool.ExecContext
// then releases the conn through its defer) must leave the conn either
// unlocked or marked broken so the pool discards it. Before the fix writerMu
// stays locked: the pool takes the conn back and the next command blocks in
// writerMu.Lock, which has no ctx.
func TestSyncPollPanicLeavesConnUsableOrBroken887(t *testing.T) {
	for _, sc := range syncCalls {
		t.Run(sc.name, func(t *testing.T) {
			c := newSyncFaultConn(panicSync{})
			func() {
				defer func() { _ = recover() }()
				sc.run(c, context.Background())
			}()
			if !c.writerMu.TryLock() {
				t.Fatal("writerMu is still locked after a recovered panic: the next command blocks in writerMu.Lock for good")
			}
			c.writerMu.Unlock()
			if !c.closed.Load() {
				t.Error("conn not marked broken after a panic left its parser mid-frame: Pool.release would put it back")
			}
		})
	}
}

// The same with runtime.Goexit (t.FailNow in a callback, say): deferred calls
// run, nothing recovers.
func TestSyncPollGoexitLeavesConnUsableOrBroken887(t *testing.T) {
	for _, sc := range syncCalls {
		t.Run(sc.name, func(t *testing.T) {
			c := newSyncFaultConn(panicSync{goexit: true})
			done := make(chan struct{})
			go func() {
				defer close(done)
				sc.run(c, context.Background())
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the goroutine did not end")
			}
			if !c.writerMu.TryLock() {
				t.Fatal("writerMu is still locked after a Goexit")
			}
			c.writerMu.Unlock()
			if !c.closed.Load() {
				t.Error("conn not marked broken after a Goexit left its parser mid-frame")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// wire helpers for the dispatch probes

func pgMsg(typ byte, payload []byte) []byte {
	b := make([]byte, 5+len(payload))
	b[0] = typ
	binary.BigEndian.PutUint32(b[1:5], uint32(4+len(payload)))
	copy(b[5:], payload)
	return b
}

func pgRowDesc1() []byte {
	var p []byte
	p = binary.BigEndian.AppendUint16(p, 1)
	p = append(p, 'a', 0)
	p = binary.BigEndian.AppendUint32(p, 0)  // table oid
	p = binary.BigEndian.AppendUint16(p, 0)  // attnum
	p = binary.BigEndian.AppendUint32(p, 25) // text
	p = binary.BigEndian.AppendUint16(p, 0xffff)
	p = binary.BigEndian.AppendUint32(p, 0xffffffff)
	p = binary.BigEndian.AppendUint16(p, 0) // text format
	return pgMsg(protocol.BackendRowDescription, p)
}

// pgDataRow is a DataRow of nf fields of flen bytes each.
func pgDataRow(nf, flen int) []byte {
	p := binary.BigEndian.AppendUint16(nil, uint16(nf))
	val := make([]byte, flen)
	for i := 0; i < nf; i++ {
		p = binary.BigEndian.AppendUint32(p, uint32(flen))
		p = append(p, val...)
	}
	return pgMsg(protocol.BackendDataRow, p)
}

func pgCompleteAndReady() []byte {
	b := pgMsg(protocol.BackendCommandComplete, append([]byte("SELECT 1"), 0))
	return append(b, pgMsg(protocol.BackendReadyForQuery, []byte{'I'})...)
}

func newDispatchConn() *pgConn {
	return &pgConn{bridge: async.NewBridge(), reader: protocol.NewReader(), writer: protocol.NewWriter()}
}

// ---------------------------------------------------------------------------
// 887.26

// dispatch peeked the head request and then runs on, with no pin, while a
// failAll on another goroutine (a cancel timeout, Close) fails it, wakes the
// caller, and the caller releases the request. The window is forced without a
// hook, with a lock dispatch takes on the request's streaming channel: the
// test holds sendMu for reading (what an OnRow mid-send holds), so dispatch's
// closeRowCh at the end of the query waits there with its stale head. While it
// waits, failAll finishes the request and the caller releases it. A request a
// dispatch still holds must not be recycled: releasePgRequest resets its fields
// and the pool hands it to the next query, whose result dispatch then
// completes (finish) silently.
func TestReleaseWhileDispatchHoldsTheHeadDoesNotRecycleIt887(t *testing.T) {
	req := c887PooledRequest(t)
	req.rowCh = make(chan [][]byte, 2)
	req.colsCh = make(chan struct{})
	c := newDispatchConn()
	c.enqueue(req)

	req.sendMu.RLock()
	locked := true
	unlock := func() {
		if locked {
			locked = false
			req.sendMu.RUnlock()
		}
	}
	t.Cleanup(unlock)

	dispatched := make(chan struct{})
	go func() {
		defer close(dispatched)
		c.onRecv(pgCompleteAndReady())
	}()
	// dispatch has peeked the head and is inside closeRowCh (rowChClosing is set
	// before it waits for sendMu) once this turns true.
	deadline := time.Now().Add(5 * time.Second)
	for {
		req.chMu.Lock()
		closing := req.rowChClosing
		req.chMu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			unlock()
			t.Fatal("dispatch never reached the end of the query")
		}
		runtime.Gosched()
	}

	// The cross-goroutine failAll and the caller's release.
	c.failAll(errors.New("cancel timeout"))
	if !req.finished.Load() {
		unlock()
		t.Fatal("failAll did not finish the request: the scenario is not the one under test")
	}
	releasePgRequest(req)
	if req.ctx == nil {
		t.Error("releasePgRequest recycled a request that dispatch still holds: dispatch goes on to close its channels and finish it after the pool handed it to the next query")
	}

	unlock()
	select {
	case <-dispatched:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not return")
	}
}

// ---------------------------------------------------------------------------
// 887.23

// failAll from another goroutine wakes the caller while dispatch is still in
// the middle of appending a DataRow to req.rows; the caller reads req.rows and
// req.columns the way waitForQueryRows' done branch does. The race detector
// reports the append against that read (any iteration where dispatch peeked the
// head before failAll took the queue). The DataRow is large (2000 fields of 1
// KiB) so the append takes long enough to overlap.
func TestFailAllWakeRacesDispatchAppendingRows887(t *testing.T) {
	row := pgDataRow(2000, 1024)
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		c := newDispatchConn()
		req := c887PooledRequest(t)
		c.enqueue(req)
		c.onRecv(pgRowDesc1())

		wg.Add(1)
		go func() {
			defer wg.Done()
			c.onRecv(row)
		}()
		for spin := 0; spin < i*40; spin++ {
			runtime.Gosched()
		}
		c.failAll(errors.New("conn closed"))
		select {
		case <-req.doneCh:
		case <-time.After(5 * time.Second):
			t.Fatal("failAll did not wake the caller")
		}
		// waitForQueryRows, done branch with req.err != nil.
		if len(req.rows) > 0 {
			_ = req.columns
		}
		wg.Wait()
	}
}
