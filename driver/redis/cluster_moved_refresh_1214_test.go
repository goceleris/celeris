package redis

// celeris#1214: the cluster command path ran its own CLUSTER SLOTS on every
// -MOVED reply (doWithRetry), on every primary network error, and once per
// pipeline batch. When a hot slot moves, every in-flight command on it gets its
// own -MOVED and sends its own CLUSTER SLOTS to the same probe node, and each
// applyTopology takes the table lock to rebuild 16384 slots. The fix reads the
// table with refreshTopologySince, with the mark taken when the reason to look
// (the MOVED reply, the network error) was received.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// movedHerdBound is the most CLUSTER SLOTS a herd of 200 commands may cost. The
// calls share a read only if it began after the mark of each, and the MOVED
// replies arrive over some time, so the count is a few waves, not exactly one.
// Main sends one per command (about 200).
const movedHerdBound = 20

// herdSlotMove builds a 3-node fake cluster whose slot of key has moved from
// from to to, with a client that has not learnt of it yet.
func herdSlotMove(t *testing.T, key string, staleReplies int) (cl *rpCluster, cc *ClusterClient, from, to *rpNode) {
	t.Helper()
	cl = newRPCluster(t, 3)
	cc = newRPClusterClient(t, cl)
	slot := int(Slot(key))
	from = cl.owner(slot)
	to = cl.nodes[0]
	if to == from {
		to = cl.nodes[1]
	}
	cl.moveSlot(slot, to, staleReplies)
	return cl, cc, from, to
}

// runHerd runs n concurrent calls of fn released together and returns the first
// error.
func runHerd(n int, fn func() error) error {
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- fn()
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// 200 concurrent GETs on a slot that has moved: each gets -MOVED. The topology is
// read a few times for all of them, not once each, and every GET still goes to
// the address its -MOVED named. With staleReplies the probe node keeps showing
// the old table (a node that has not heard of the move): the redirect must still
// reach the new owner, whatever the refreshed table says.
func TestMovedHerdRefreshesTheTopologyAFewTimes1214(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale int
	}{{"fresh-table", 0}, {"probe-node-still-stale", 1 << 20}} {
		t.Run(tc.name, func(t *testing.T) {
			const n = 200
			const key = "{herd}k"
			cl, cc, from, to := herdSlotMove(t, key, tc.stale)
			before := cl.slotsReplies.Load()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := runHerd(n, func() error {
				v, err := cc.Get(ctx, key)
				if err != nil {
					return err
				}
				if v != "v" {
					return fmt.Errorf("GET = %q, want the owner's value", v)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			got := cl.slotsReplies.Load() - before
			t.Logf("%d GETs on a moved slot (%s): %d CLUSTER SLOTS; %d GETs reached the old owner, %d the new one",
				n, tc.name, got, from.getCmds.Load(), to.getCmds.Load())
			if got > movedHerdBound {
				t.Errorf("%d CLUSTER SLOTS for %d GETs that hit one slot move, want at most %d", got, n, movedHerdBound)
			}
			if g := to.getCmds.Load(); g < n {
				t.Errorf("the new owner got %d GETs, want %d: the redirect did not go to the MOVED address", g, n)
			}
			if tc.stale == 0 {
				if r := cc.route(Slot(key)); r == nil || r.addr != to.addr {
					t.Errorf("the table still routes the slot to the old owner after the refreshes")
				}
			}
		})
	}
}

// 50 pipelines whose GETs hit the moved slot: one topology read per batch is
// still one per pipeline. The retry goes to the new owner.
func TestMovedHerdOfPipelinesRefreshesTheTopologyAFewTimes1214(t *testing.T) {
	const n = 50
	const key = "{pipeherd}k"
	cl, cc, _, to := herdSlotMove(t, key, 0)
	before := cl.slotsReplies.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runHerd(n, func() error {
		p := cc.Pipeline()
		p.Get(key)
		vals, errs := p.Exec(ctx)
		if errs[0] != nil {
			return errs[0]
		}
		if string(vals[0].Str) != "v" {
			return fmt.Errorf("pipelined GET = %q, want the owner's value", vals[0].Str)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := cl.slotsReplies.Load() - before
	t.Logf("%d pipelines on a moved slot: %d CLUSTER SLOTS", n, got)
	if got > movedHerdBound/2 {
		t.Errorf("%d CLUSTER SLOTS for %d pipelines that hit one slot move, want at most %d", got, n, movedHerdBound/2)
	}
	if g := to.getCmds.Load(); g < n {
		t.Errorf("the new owner got %d GETs, want %d", g, n)
	}
}

// 100 concurrent GETs on a slot whose primary dies and is replaced: each gets a
// network error and refreshes the table (the primary-network-error path). The
// topology is read a few times for all of them.
func TestPrimaryLostHerdRefreshesTheTopologyAFewTimes1214(t *testing.T) {
	const n = 100
	const key = "{lost}k"
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	slot := int(Slot(key))
	from := cl.owner(slot)
	to := cl.nodes[0]
	if to == from {
		to = cl.nodes[1]
	}
	if v, err := cc.Get(rpCtx(t), key); err != nil || v != "v" {
		t.Fatalf("GET before the failure = %q, %v", v, err)
	}
	from.kill()
	cl.failover(from, to, 0)
	before := cl.slotsReplies.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runHerd(n, func() error {
		v, err := cc.Get(ctx, key)
		if err != nil {
			return err
		}
		if v != "v" {
			return fmt.Errorf("GET = %q, want the owner's value", v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := cl.slotsReplies.Load() - before
	t.Logf("%d GETs on a slot whose primary died: %d CLUSTER SLOTS", n, got)
	if got > movedHerdBound {
		t.Errorf("%d CLUSTER SLOTS for %d GETs that lost the same primary, want at most %d", got, n, movedHerdBound)
	}
}

// The sharing is by trigger: a command whose reason to look (its -MOVED, its
// network error) arrives while an older read is in flight must not adopt that
// read. Its table was taken before the move (held here, as a slow reply would
// be), so adopting it would route the next command to the old owner. The command
// reads the table again, and the table it adopts is the new one.
func TestMovedCommandDoesNotAdoptARunningReadOlderThanItsMoved1214(t *testing.T) {
	for _, tc := range []string{"moved", "primary-lost"} {
		t.Run(tc, func(t *testing.T) {
			const key = "{stale}k"
			cl := newRPCluster(t, 3)
			cc := newRPClusterClient(t, cl)
			slot := int(Slot(key))
			from := cl.owner(slot)
			to := cl.nodes[0]
			if to == from {
				to = cl.nodes[1]
			}
			if v, err := cc.Get(rpCtx(t), key); err != nil || v != "v" {
				t.Fatalf("GET before the move = %q, %v", v, err)
			}

			if tc == "primary-lost" {
				// The held read must not ask the node that is about to die (it
				// would then ask the next node, a second CLUSTER SLOTS that is
				// not the command's): mark that node failing, as a read that met
				// it would, so the probe picks another.
				cc.mu.RLock()
				cc.nodes[from.addr].failing.Store(true)
				cc.mu.RUnlock()
			}
			waitHeld, release := holdNextSlotsRead(t)
			r1 := make(chan error, 1)
			go func() { r1 <- cc.refreshTopology(rpCtx(t)) }()
			waitHeld() // its table, from before the move, is taken; the reply is held

			if tc == "primary-lost" {
				from.kill()
				cl.failover(from, to, 0)
			} else {
				cl.moveSlot(slot, to, 0)
			}
			before := cl.slotsReplies.Load()
			// Short context: if the command waits for the held read, it fails at
			// 1 s instead of hanging.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			v, err := cc.Get(ctx, key)
			if err != nil || v != "v" {
				t.Fatalf("GET after the move = %q, %v: it waited for a read older than its %s", v, err, tc)
			}
			if got := cl.slotsReplies.Load() - before; got != 1 {
				t.Errorf("%d CLUSTER SLOTS for the command, want one of its own", got)
			}
			if r := cc.route(Slot(key)); r == nil || r.addr != to.addr {
				t.Errorf("the table still routes the slot to the old owner: the command adopted the older read")
			}
			release()
			if err := <-r1; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// otherNode returns a node of cl that is none of avoid.
func otherNode(cl *rpCluster, avoid ...*rpNode) *rpNode {
	for _, n := range cl.nodes {
		ok := true
		for _, a := range avoid {
			ok = ok && n != a
		}
		if ok {
			return n
		}
	}
	return nil
}

// holdNextSlotsRead makes the next CLUSTER SLOTS reply wait after its table is
// taken. It returns a func that waits until that has happened (and fails the test
// if it does not) and a func that releases the reply (also run at the end of the
// test).
func holdNextSlotsRead(t *testing.T) (waitHeld func(), release func()) {
	t.Helper()
	s := make(chan struct{})
	r := make(chan struct{})
	var once sync.Once
	rel := func() { once.Do(func() { close(r) }) }
	h := func() { close(s); <-r }
	rpSlotsHold.Store(&h)
	t.Cleanup(func() {
		rel()
		rpSlotsHold.Store(nil)
	})
	return func() {
		t.Helper()
		select {
		case <-s:
		case <-time.After(3 * time.Second):
			t.Fatal("no CLUSTER SLOTS read was started: the command did not read the topology after its reply")
		}
	}, rel
}

// An event is resolved once a read that started after it has ended: the next
// -MOVED for the same slot and node is a new event and needs a read of its own.
// The probe node here still shows the old table for the first command (so the
// read that follows its -MOVED ends without the move in it); the second command
// meets the same -MOVED once the probe node knows, and must not take the first
// one's finished read for a read that follows its own reply. (Passes on main, by
// design: it guards the expiry of the shared event. A variant that never
// resolves an event fails it.)
func TestMovedEventIsResolvedByTheReadThatFollowsIt1214(t *testing.T) {
	const key = "{expiry}k"
	cl, cc, _, to := herdSlotMove(t, key, 1<<20)
	ctx := rpCtx(t)
	if v, err := cc.Get(ctx, key); err != nil || v != "v" {
		t.Fatalf("first GET = %q, %v", v, err)
	}
	if r := cc.route(Slot(key)); r == nil || r.addr == to.addr {
		t.Fatal("the probe node should still have shown the old table")
	}
	cl.mu.Lock()
	cl.stale = 0 // the probe node knows of the move now
	cl.mu.Unlock()
	before := cl.slotsReplies.Load()
	if v, err := cc.Get(ctx, key); err != nil || v != "v" {
		t.Fatalf("second GET = %q, %v", v, err)
	}
	if got := cl.slotsReplies.Load() - before; got != 1 {
		t.Errorf("%d CLUSTER SLOTS for the second -MOVED, want one of its own", got)
	}
	if r := cc.route(Slot(key)); r == nil || r.addr != to.addr {
		t.Errorf("the table still routes the slot to the old owner: the second command used the finished read of the first")
	}
}

// -MOVED for the same slot to another node is another event, not the one still
// open: the slot moved again while a read that follows the first was in flight.
// That read's table (held here) names the first target, so the second command
// must read again. (A variant that keys the event on the slot alone joins it.)
func TestMovedToAnotherNodeIsANewEvent1214(t *testing.T) {
	const key = "{again}k"
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	slot := int(Slot(key))
	a := cl.owner(slot)
	b := otherNode(cl, a)
	c := otherNode(cl, a, b)
	cl.moveSlot(slot, b, 0)

	waitHeld, release := holdNextSlotsRead(t)
	first := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		v, err := cc.Get(ctx, key)
		if err == nil && v != "v" {
			err = fmt.Errorf("GET = %q", v)
		}
		first <- err
	}()
	waitHeld() // the first command's -MOVED (to b) is seen, its read has taken b's table
	cl.moveSlot(slot, c, 0)
	before := cl.slotsReplies.Load()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if v, err := cc.Get(ctx, key); err != nil || v != "v" {
		t.Fatalf("GET after the second move = %q, %v: it waited for the read that names the first target", v, err)
	}
	if got := cl.slotsReplies.Load() - before; got != 1 {
		t.Errorf("%d CLUSTER SLOTS for the second move, want one of its own", got)
	}
	if r := cc.route(Slot(key)); r == nil || r.addr != c.addr {
		t.Errorf("the table does not name the node the slot moved to last")
	}
	release()
	if err := <-first; err != nil {
		t.Errorf("the first command: %v", err)
	}
}

// A batch with two moved slots needs a read that follows both -MOVED replies, not
// just the first one's: the read that is still in flight follows the first
// (shared event) but its table, taken before the second slot moved, does not have
// the second. The batch reads again, whichever slot comes first. (A variant that
// uses the mark of the first key, or of the earliest event, joins it.)
func TestPipelineOfTwoMovesNeedsAReadAfterBoth1214(t *testing.T) {
	for _, order := range []string{"old-event-first", "old-event-last"} {
		t.Run(order, func(t *testing.T) {
			const k1, k2 = "{pipe-one}k", "{pipe-two}k"
			cl := newRPCluster(t, 3)
			cc := newRPClusterClient(t, cl)
			s1, s2 := int(Slot(k1)), int(Slot(k2))
			o1, o2 := cl.owner(s1), cl.owner(s2)
			cl.moveSlot(s1, otherNode(cl, o1), 0)

			waitHeld, release := holdNextSlotsRead(t)
			first := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, err := cc.Get(ctx, k1) // -MOVED of slot 1: its read is held
				first <- err
			}()
			waitHeld()
			cl.moveSlot(s2, otherNode(cl, o2), 0) // after that read took its table

			keys := []string{k1, k2}
			if order == "old-event-last" {
				keys = []string{k2, k1}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			p := cc.Pipeline()
			for _, k := range keys {
				p.Get(k)
			}
			vals, errs := p.Exec(ctx)
			for i := range keys {
				if errs[i] != nil || string(vals[i].Str) != "v" {
					t.Errorf("pipelined GET %s = %q, %v: the batch used a read that does not have both moves", keys[i], vals[i].Str, errs[i])
				}
			}
			release()
			if err := <-first; err != nil {
				t.Errorf("the first command: %v", err)
			}
		})
	}
}
