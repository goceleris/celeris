package redis

// celeris#1214: the cluster command path ran its own CLUSTER SLOTS on every
// -MOVED reply (doWithRetry), on every primary network error, and once per
// pipeline batch. When a hot slot moves, every in-flight command on it gets its
// own -MOVED and sends its own CLUSTER SLOTS to the same probe node, and each
// applyTopology takes the table lock to rebuild 16384 slots. The fix reads the
// table with refreshTopologySince, with a mark taken by event (eventMarkLocked):
// the commands that got the same -MOVED (same slot, same target), or lost the same
// primary, share one read that started after the first of their replies. (A plain
// topoMark taken at each reply shares nothing here: every command registers its
// read right after taking its mark, so the next mark is already past it.)

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
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

// goroutinesIn counts the goroutines whose stack mentions fn.
func goroutinesIn(fn string) int {
	buf := make([]byte, 1<<22)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, fn) {
			n++
		}
	}
	return n
}

// keyOwnedBy returns a key (tagged by prefix) whose slot is owned by one of the
// nodes of want and is not in used.
func keyOwnedBy(t *testing.T, cl *rpCluster, prefix string, want func(*rpNode) bool, used ...int) string {
	t.Helper()
	for i := 0; i < 10000; i++ {
		k := fmt.Sprintf("{%s%d}k", prefix, i)
		s := int(Slot(k))
		ok := want(cl.owner(s))
		for _, u := range used {
			ok = ok && u != s
		}
		if ok {
			return k
		}
	}
	t.Fatal("no key found")
	return ""
}

// The exact version of the herd test: the first read is HELD after its table is
// taken, until every one of the 200 commands has its -MOVED and waits for a read.
// They all share that one read: exactly one CLUSTER SLOTS, whatever the speed of
// the machine. (Main: one per command.)
func TestMovedHerdSharesTheReadInFlightExactly1214(t *testing.T) {
	const n = 200
	const key = "{exact}k"
	cl, cc, from, _ := herdSlotMove(t, key, 0)
	waitHeld, release := holdNextSlotsRead(t)
	before := cl.slotsReplies.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := make(chan error, 1)
	go func() {
		res <- runHerd(n, func() error {
			v, err := cc.Get(ctx, key)
			if err == nil && v != "v" {
				err = fmt.Errorf("GET = %q", v)
			}
			return err
		})
	}()
	waitHeld()
	// Every command has its -MOVED and is waiting for the read (the leader's
	// goroutine and the n-1 that joined it all sit in refreshTopologySince).
	poll(t, "every command waiting for the read", 5*time.Second, func() bool {
		return from.getMoved.Load() >= n && goroutinesIn("(*ClusterClient).refreshTopologySince") >= n
	})
	release()
	if err := <-res; err != nil {
		t.Fatal(err)
	}
	if got := cl.slotsReplies.Load() - before; got != 1 {
		t.Errorf("%d CLUSTER SLOTS for %d commands that all waited for one read, want exactly 1", got, n)
	}
}

// An event stays resolved: a slot that went to B (the command's read ended), came
// back to A seen only by a background refresh, and goes to B again. The command
// that gets the new -MOVED while another read, started before that move, is in
// flight must not wait for it (its table is from before the move): it reads again.
// (A variant that looks only at the latest read re-opens the old event while any
// newer read runs and waits for it.)
func TestMovedEventStaysResolvedWhenALaterReadIsRunning1214(t *testing.T) {
	const key = "{sticky}k"
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	slot := int(Slot(key))
	a := cl.owner(slot)
	b := otherNode(cl, a)
	ctx := rpCtx(t)
	cl.moveSlot(slot, b, 0)
	if v, err := cc.Get(ctx, key); err != nil || v != "v" { // event (slot, b), resolved by its read
		t.Fatalf("first GET = %q, %v", v, err)
	}
	cl.moveSlot(slot, a, 0)
	if err := cc.refreshTopology(ctx); err != nil { // a background refresh learns it; no -MOVED seen
		t.Fatal(err)
	}
	waitHeld, release := holdNextSlotsRead(t)
	r := make(chan error, 1)
	go func() { r <- cc.refreshTopology(rpCtx(t)) }() // a read in flight, its table has the slot on a
	waitHeld()
	cl.moveSlot(slot, b, 0)
	before := cl.slotsReplies.Load()
	gctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if v, err := cc.Get(gctx, key); err != nil || v != "v" {
		t.Fatalf("GET after the second move = %q, %v: it waited for a read that started before its move", v, err)
	}
	if got := cl.slotsReplies.Load() - before; got != 1 {
		t.Errorf("%d CLUSTER SLOTS for the command, want one of its own", got)
	}
	release()
	if err := <-r; err != nil {
		t.Fatal(err)
	}
}

// Events are per slot: two different slots that move to the SAME node are two
// events. The second command must not take the read held for the first (its table
// is from before the second slot moved). (A variant that keys the event on the
// target alone shares it.)
func TestTwoSlotsMovedToTheSameNodeDoNotShareARead1214(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	b := cl.nodes[0]
	notB := func(n *rpNode) bool { return n != b }
	k1 := keyOwnedBy(t, cl, "twoa", notB)
	k2 := keyOwnedBy(t, cl, "twob", notB, int(Slot(k1)))
	cl.moveSlot(int(Slot(k1)), b, 0)

	waitHeld, release := holdNextSlotsRead(t)
	first := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := cc.Get(ctx, k1) // -MOVED of slot 1 to b: its read is held
		first <- err
	}()
	waitHeld()
	cl.moveSlot(int(Slot(k2)), b, 0) // after that read took its table; same target
	before := cl.slotsReplies.Load()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if v, err := cc.Get(ctx, k2); err != nil || v != "v" {
		t.Fatalf("GET on the second slot = %q, %v: it waited for the read held for the first slot", v, err)
	}
	if got := cl.slotsReplies.Load() - before; got != 1 {
		t.Errorf("%d CLUSTER SLOTS for the second slot, want one of its own", got)
	}
	release()
	if err := <-first; err != nil {
		t.Errorf("the first command: %v", err)
	}
}

// A lost primary is its own event: a command that loses a primary while a read
// held for a -MOVED is in flight must not take that read (its table is from before
// the primary died).
func TestLostPrimaryDoesNotTakeAReadHeldForAMoved1214(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	k1 := keyOwnedBy(t, cl, "lostmv", func(*rpNode) bool { return true })
	s1 := int(Slot(k1))
	o1 := cl.owner(s1)
	b := otherNode(cl, o1)
	x := otherNode(cl, o1, b) // the node that will die
	k2 := keyOwnedBy(t, cl, "lostx", func(n *rpNode) bool { return n == x }, s1)
	cl.moveSlot(s1, b, 0)
	// The held read must not ask the node that is about to die.
	cc.mu.RLock()
	cc.nodes[x.addr].failing.Store(true)
	cc.mu.RUnlock()

	waitHeld, release := holdNextSlotsRead(t)
	first := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := cc.Get(ctx, k1)
		first <- err
	}()
	waitHeld()
	x.kill()
	cl.failover(x, o1, 0)
	before := cl.slotsReplies.Load()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if v, err := cc.Get(ctx, k2); err != nil || v != "v" {
		t.Fatalf("GET on the lost primary's slot = %q, %v: it waited for the read held for a -MOVED", v, err)
	}
	if got := cl.slotsReplies.Load() - before; got != 1 {
		t.Errorf("%d CLUSTER SLOTS for the lost primary, want one of its own", got)
	}
	release()
	if err := <-first; err != nil {
		t.Errorf("the first command: %v", err)
	}
}

// A pipeline in which nothing fails pays nothing for the MOVED scan: the scan
// must not allocate (an error pointer declared before the nil check escapes to the
// heap and costs one allocation per command of every pipeline).
func TestRefreshForMovedAllocatesNothingWhenNothingFailed1214(t *testing.T) {
	cl := newRPCluster(t, 1)
	cc := newRPClusterClient(t, cl)
	cp := cc.Pipeline()
	errs := make([]error, 100)
	if got := testing.AllocsPerRun(100, func() { cp.refreshForMoved(context.Background(), errs) }); got != 0 {
		t.Errorf("refreshForMoved allocated %v times for 100 commands that did not fail, want 0", got)
	}
}

// The event table costs nothing until the first -MOVED: the client is the two slot
// tables of main plus a small rest, and the 16384-entry table of events is
// allocated by the first -MOVED, not by NewClusterClient (it would double the
// client, and the GC scans it).
func TestEventTableIsAllocatedByTheFirstMoved1214(t *testing.T) {
	var cc ClusterClient
	slotTables := unsafe.Sizeof(cc.slots) + unsafe.Sizeof(cc.replicas)
	if rest := unsafe.Sizeof(cc) - slotTables; rest > 2048 {
		t.Errorf("ClusterClient is %d bytes beyond its slot tables, want a small constant (the event table must not be inline)", rest)
	}
	const key = "{table}k"
	cl, c, _, _ := herdSlotMove(t, key, 0)
	c.refMu.Lock()
	idle := c.refMoved
	c.refMu.Unlock()
	if idle != nil {
		t.Error("a client that has met no -MOVED holds the event table")
	}
	if v, err := c.Get(rpCtx(t), key); err != nil || v != "v" {
		t.Fatalf("GET = %q, %v", v, err)
	}
	c.refMu.Lock()
	after := c.refMoved
	c.refMu.Unlock()
	if after == nil {
		t.Error("the first -MOVED did not allocate the event table")
	}
	_ = cl
}

// refDone only goes up: a read that started earlier and ends after a newer one
// must not lower it (the newer read already resolved the events before it).
func TestRefDoneNeverGoesDown1214(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	ctx := rpCtx(t)
	waitHeld, release := holdNextSlotsRead(t)
	r1 := make(chan error, 1)
	go func() { r1 <- cc.refreshTopology(ctx) }() // the older read, held
	waitHeld()
	if err := cc.refreshTopology(ctx); err != nil { // a newer one ends first
		t.Fatal(err)
	}
	done := func() uint64 {
		cc.refMu.Lock()
		defer cc.refMu.Unlock()
		return cc.refDone
	}
	newer := done()
	release()
	if err := <-r1; err != nil {
		t.Fatal(err)
	}
	if got := done(); got != newer {
		t.Errorf("refDone = %d after the older read ended, want %d: it went down", got, newer)
	}
}
