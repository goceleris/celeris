package postgres

// RULE 74 micro-benchmarks for celeris#887 items 22, 23 and 26: the per-message
// code the changes touch. They use only symbols that exist on main, so benchstat
// compares main and the fix directly.
//
//	SimpleExecSync887   one simpleExec per op over a fake sync loop that answers at once:
//	                    pollSync's deferred unlock (22), dispatch's pin (26)
//	QuerySync887        one 1-column, 8-row simpleQuery per op, same loop: adds OnRowDesc and
//	                    8 OnRow (23's doneMu guard)
//	DispatchBuffered887 the 8-row reply fed through onRecv with no query around it

import (
	"context"
	"runtime"
	"testing"

	"github.com/goceleris/celeris/driver/internal/async"
	"github.com/goceleris/celeris/internal/driver/postgres/protocol"
)

// replySync answers every WriteAndPoll by feeding reply to onRecv, the way the
// real worker does when the response is already there.
type replySync struct{ reply []byte }

func (r replySync) WriteAndPoll(_ int, _ []byte, _ []byte, onRecv func([]byte)) (bool, error) {
	onRecv(r.reply)
	return true, nil
}

func benchRows8Reply() []byte {
	b := pgRowDesc1()
	for i := 0; i < 8; i++ {
		b = append(b, pgDataRow(1, 16)...)
	}
	return append(b, pgCompleteAndReady()...)
}

func newReplyConn(reply []byte) *pgConn {
	c := &pgConn{
		bridge:   async.NewBridge(),
		writer:   protocol.NewWriter(),
		reader:   protocol.NewReader(),
		syncLoop: &pgSyncLoop{sync: replySync{reply: reply}},
		syncBuf:  make([]byte, 1024),
	}
	c.onRecvFn = c.onRecv
	return c
}

func BenchmarkSimpleExecSync887(b *testing.B) {
	c := newReplyConn(pgCompleteAndReady())
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := c.simpleExec(ctx, "SELECT 1"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQuerySync887(b *testing.B) {
	c := newReplyConn(benchRows8Reply())
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		rows, err := c.simpleQuery(ctx, "SELECT a FROM t")
		if err != nil {
			b.Fatal(err)
		}
		_ = rows.Close()
	}
}

func BenchmarkDispatchBuffered887(b *testing.B) {
	c := newDispatchConn()
	reply := benchRows8Reply()
	b.ReportAllocs()
	for b.Loop() {
		req := acquirePgRequest()
		req.ctx = context.Background()
		req.kind = reqSimple
		c.enqueue(req)
		c.onRecv(reply)
		releasePgRequest(req)
	}
}

// BenchmarkDispatchAsync887 is the async shape: the "loop" goroutine runs
// onRecv while the caller, on another goroutine, spins on doneAtom, releases the
// request and acquires the next. allocs/op shows how often the caller's release
// meets dispatch's pin (a pinned request is dropped, so the next acquire
// allocates). It uses only symbols that exist on main.
func BenchmarkDispatchAsync887(b *testing.B) {
	c := newDispatchConn()
	reply := benchRows8Reply()
	kick := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		for range kick {
			c.onRecv(reply)
		}
		close(stop)
	}()
	b.ReportAllocs()
	for b.Loop() {
		req := acquirePgRequest()
		req.ctx = context.Background()
		req.kind = reqSimple
		c.enqueue(req)
		kick <- struct{}{}
		for req.doneAtom.Load() == 0 {
			runtime.Gosched()
		}
		releasePgRequest(req)
	}
	close(kick)
	<-stop
}
