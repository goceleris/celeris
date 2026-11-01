package redis

// celeris#1203: every sharded reroute attempt ran its own CLUSTER SLOTS. One
// SETSLOT that hit N sharded PubSubs sent N of them to the same probe node, and
// each backoff step of an outage sent them again (the review of #1154 put 1000
// PubSubs at the 5 s cap at 200 CLUSTER SLOTS per second).

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// 200 sharded PubSubs on one node, one slot move: the topology is read about
// once for all of them, not once each.
func TestShardedRerouteOfManyPubSubsRefreshesTheTopologyOnce1203(t *testing.T) {
	const n = 200
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const tag = "{many}"
	slot := int(Slot(tag))
	from := cl.owner(slot)
	to := cl.nodes[0]
	if to == from {
		to = cl.nodes[1]
	}
	pss := make([]*PubSub, 0, n)
	for i := 0; i < n; i++ {
		ps, err := cc.SSubscribe(rpCtx(t), fmt.Sprintf("%s%d", tag, i))
		if err != nil {
			t.Fatal(err)
		}
		pss = append(pss, ps)
	}
	t.Cleanup(func() {
		for _, ps := range pss {
			_ = ps.Close()
		}
	})
	poll(t, "every PubSub on the owner", 10*time.Second, func() bool { return from.subscribers("ssub") == n })

	before := cl.slotsReplies.Load()
	cl.moveSlot(slot, to, 0)
	poll(t, "every PubSub on the new owner", 20*time.Second, func() bool { return to.subscribers("ssub") == n })
	got := cl.slotsReplies.Load() - before
	t.Logf("%d sharded PubSubs, one slot move: %d CLUSTER SLOTS", n, got)
	if got > 5 {
		t.Errorf("%d CLUSTER SLOTS for one slot move that hit %d PubSubs, want about one (at most 5)", got, n)
	}
}

// The sharing is by trigger, not by a clock window: a reroute never reuses a
// refresh that began before its own trigger. A refresh done just before the move
// names the old owner; the PubSub that the move hits must read the table again
// and go straight to the new owner (a reuse would send it to the old owner, which
// answers -MOVED, and cost a backoff step).
func TestShardedRerouteDoesNotReuseARefreshOlderThanItsTrigger1203(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const ch = "{fresh}x"
	slot := int(Slot(ch))
	from := cl.owner(slot)
	to := cl.nodes[0]
	if to == from {
		to = cl.nodes[1]
	}
	ps, err := cc.SSubscribe(rpCtx(t), ch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return from.subscribers("ssub") == 1 })

	// A refresh that completes just before the move.
	if err := cc.refreshTopology(rpCtx(t)); err != nil {
		t.Fatal(err)
	}
	sent := from.subCmds.Load()
	cl.moveSlot(slot, to, 0)
	poll(t, "SSUBSCRIBE on the new owner", 10*time.Second, func() bool { return to.subscribers("ssub") == 1 })
	if got := from.subCmds.Load() - sent; got != 0 {
		t.Errorf("the old owner got %d SSUBSCRIBE after the move: the reroute used the table read before it", got)
	}
}

// An outage that lasts: the new owner is gone, 100 PubSubs retry at the
// backoff (50 ms doubling). The reroute attempts are shared as well as the first one: the
// CLUSTER SLOTS count follows the backoff steps, not the PubSub count.
func TestShardedRerouteAttemptsOfAnOutageShareTheirRefreshes1203(t *testing.T) {
	const n = 100
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const tag = "{outage}"
	slot := int(Slot(tag))
	from := cl.owner(slot)
	to := cl.nodes[0]
	if to == from {
		to = cl.nodes[1]
	}
	pss := make([]*PubSub, 0, n)
	for i := 0; i < n; i++ {
		ps, err := cc.SSubscribe(rpCtx(t), fmt.Sprintf("%s%d", tag, i))
		if err != nil {
			t.Fatal(err)
		}
		pss = append(pss, ps)
	}
	t.Cleanup(func() {
		for _, ps := range pss {
			_ = ps.Close()
		}
	})
	poll(t, "every PubSub on the owner", 10*time.Second, func() bool { return from.subscribers("ssub") == n })

	before := cl.slotsReplies.Load()
	cl.moveSlot(slot, to, 0)
	to.kill() // the table names an owner that does not answer
	time.Sleep(time.Second)
	got := cl.slotsReplies.Load() - before
	t.Logf("%d PubSubs, 1 s of an outage (backoff 50, 100, 200, 400 ms ...): %d CLUSTER SLOTS", n, got)
	if got > 20 {
		t.Errorf("%d CLUSTER SLOTS in 1 s for %d PubSubs, want at most 20: the attempts are not shared", got, n)
	}
}

// A refresh that is still RUNNING when the move lands began before the PubSub's
// trigger: its table was taken before the move (held here, as a slow reply
// would be). The reroute must not join it, whatever its state: it reads again.
// (A variant that applies the trigger rule to finished reads only joins this one,
// and the old owner gets an SSUBSCRIBE.)
func TestShardedRerouteDoesNotJoinARunningRefreshOlderThanItsTrigger1203(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const ch = "{inflight}x"
	slot := int(Slot(ch))
	from := cl.owner(slot)
	to := cl.nodes[0]
	if to == from {
		to = cl.nodes[1]
	}
	ps, err := cc.SSubscribe(rpCtx(t), ch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return from.subscribers("ssub") == 1 })

	snap := make(chan struct{})
	release := make(chan struct{})
	h := func() { close(snap); <-release }
	rpSlotsHold.Store(&h)
	r1 := make(chan error, 1)
	go func() { r1 <- cc.refreshTopology(rpCtx(t)) }()
	<-snap // its table, from before the move, is taken; the reply is held
	sent := from.subCmds.Load()
	cl.moveSlot(slot, to, 0)
	time.Sleep(400 * time.Millisecond) // the PubSub picks while that read runs
	close(release)
	if err := <-r1; err != nil {
		t.Fatal(err)
	}
	poll(t, "SSUBSCRIBE on the new owner", 10*time.Second, func() bool { return to.subscribers("ssub") == 1 })
	if got := from.subCmds.Load() - sent; got != 0 {
		t.Errorf("the old owner got %d SSUBSCRIBE after the move: the reroute used a running read older than its trigger", got)
	}
}

// The caller whose read others joined has its context cancelled: the read ends
// with its context error, which says nothing about the cluster. The joiner must
// not return it: it reads again (one extra CLUSTER SLOTS) and succeeds.
func TestRefreshJoinerReadsAgainWhenTheReadItJoinedWasCancelled1203(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	since := time.Now()
	snap := make(chan struct{})
	release := make(chan struct{})
	h := func() { close(snap); <-release }
	rpSlotsHold.Store(&h)
	defer close(release)

	actx, acancel := context.WithCancel(context.Background())
	aerr := make(chan error, 1)
	go func() { aerr <- cc.refreshTopologySince(actx, since) }()
	<-snap // A's read runs, its reply held
	before := cl.slotsReplies.Load()
	berr := make(chan error, 1)
	go func() { berr <- cc.refreshTopologySince(rpCtx(t), since) }()
	time.Sleep(50 * time.Millisecond) // B joined A's read
	acancel()
	if err := <-aerr; err == nil {
		t.Error("the caller that was cancelled got no error")
	}
	select {
	case err := <-berr:
		if err != nil {
			t.Errorf("the joiner failed with the other caller's cancellation: %v", err)
		}
		if got := cl.slotsReplies.Load() - before; got != 1 {
			t.Errorf("%d CLUSTER SLOTS for the joiner, want one of its own", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the joiner is stuck")
	}
}

// A probe node that is down but not marked failing yet (the first read to meet it
// is the one that finds out) must not fail the read: it asks the next node. One
// failed read is shared by every PubSub it serves, so the first must not be lost
// to a node the table of the others could have come from.
func TestTopologyReadTriesTheNextNodeWhenTheProbeIsDown1203(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	dead := cl.nodes[2]
	dead.kill()
	// The only node anyNode would pick first is the dead one.
	cc.mu.RLock()
	for _, n := range cc.nodes {
		n.failing.Store(n.addr != dead.addr)
	}
	cc.mu.RUnlock()
	if err := cc.refreshTopology(rpCtx(t)); err != nil {
		t.Fatalf("refreshTopology with a dead probe node and live ones: %v", err)
	}
	if cc.route(0) == nil {
		t.Fatal("the table is empty")
	}
}
