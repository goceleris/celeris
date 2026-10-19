//go:build linux

package epoll

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// celeris#885 item 12 (from #819 item 2). StartWithContext's watcher hands the
// loops the shutdown budget (Engine.Shutdown stores it in drainBudget) when
// its context is cancelled, and the loops start their send drain on the same
// cancel. A handler released before the watcher has stored the budget gets a
// drain that begins with no budget at all: sendDrainWait must then read the
// budget again each time it is asked, so that the drain extends to it once it
// arrives. A drain that read the budget once at its start would keep to the
// 250 ms floor, and close the conn with most of its response still queued.
//
// The unit makes the race deterministic. The rig's drainBudget is empty when
// the drain begins, and the budget is stored by the first wait of the drain
// itself (SetDrainPollHook), so it always arrives after the drain began. The
// peer starts reading 600 ms later, well past the floor: the whole response
// reaches it only if the drain went on.

func TestShutdownSendDrainExtendsToABudgetThatArrivesAfterItBegan(t *testing.T) {
	const queued = 2 << 20
	const readAfter = 600 * time.Millisecond // past shutdownSendDrainFloor (250 ms)
	r := newDrainRig874(t, queued)
	var budget atomic.Pointer[context.Context]
	r.l.drainBudget = &budget // empty: no Shutdown has handed a budget over yet
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	var once sync.Once
	var arrived atomic.Int64 // when the drain's first wait stored the budget
	restore := SetDrainPollHook(func() {
		once.Do(func() {
			c := ctx
			budget.Store(&c)
			arrived.Store(time.Now().UnixNano())
		})
	})
	t.Cleanup(restore)

	time.AfterFunc(readAfter, r.startReader)
	start := time.Now()
	r.l.shutdown()
	took := time.Since(start)
	r.wait(t)

	if arrived.Load() == 0 {
		t.Fatalf("celeris885 PREMISE: the drain never waited for the socket, so no budget arrived (the peer got %d of %d bytes)",
			r.got.Load(), queued)
	}
	got := r.got.Load()
	t.Logf("celeris885 late budget: shutdown took %v, the peer, reading after %v, got %d of %d bytes", took.Round(time.Millisecond), readAfter, got, queued)
	if got != queued {
		t.Errorf("the peer got %d of %d bytes: the send drain ended at its floor instead of extending to the budget that "+
			"arrived after it began (celeris#885 item 12)", got, queued)
	}
	if took < readAfter {
		t.Errorf("shutdown returned after %v, before the peer began to read (%v): the drain was not held open by the budget", took, readAfter)
	}
}
