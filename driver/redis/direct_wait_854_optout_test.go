package redis

import (
	"context"
	"testing"
	"time"
)

// TestDirectWaitNetpollOptOutKeepsCoverage854: with the mode pinned to netpoll
// (CELERIS_REDIS_DIRECT_WAIT=netpoll, the pre-#854 path, and the only one off
// linux), the direct-mode cancel of celeris#1250, a pipeline and a Tx still do
// what they do in auto mode, and nothing waits in poll(2).
func TestDirectWaitNetpollOptOutKeepsCoverage854(t *testing.T) {
	t.Run("cancel1250", func(t *testing.T) {
		setDirectWaitForTest(t, directWaitNetpoll)
		TestDirectExecWithACancelledCtxReportsTheCancel1250(t)
	})
	t.Run("pipeline", func(t *testing.T) {
		f := startWaitFake(t)
		f.setDelay(2 * time.Millisecond)
		cl := newWaitClient(t, f.addr(), 1)
		setDirectWaitForTest(t, directWaitNetpoll)
		before := directWaitSnapshot()
		execN(t, cl, 5, 10)
		if d := directWaitSnapshot().since(before); d != (directWaitSnap{}) {
			t.Fatalf("netpoll mode counted: %+v", d)
		}
	})
	t.Run("tx", func(t *testing.T) {
		m := newMem()
		cl := directSlabClient(t, m.handler)
		setDirectWaitForTest(t, directWaitNetpoll)
		before := directWaitSnapshot()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for want := int64(1); want <= 3; want++ {
			tx, err := cl.TxPipeline(ctx)
			if err != nil {
				t.Fatal(err)
			}
			a := tx.Incr("n")
			if err := tx.Exec(ctx); err != nil {
				t.Fatal(err)
			}
			if got, err := a.Result(); err != nil || got != want {
				t.Fatalf("INCR = %d, %v, want %d", got, err, want)
			}
		}
		if d := directWaitSnapshot().since(before); d != (directWaitSnap{}) {
			t.Fatalf("netpoll mode counted: %+v", d)
		}
	})
}
