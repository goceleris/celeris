package redis

// celeris#1203: every sharded reroute attempt ran its own CLUSTER SLOTS. One
// SETSLOT that hit N sharded PubSubs sent N of them to the same probe node, and
// each backoff step of an outage sent them again (the review of #1154 put 1000
// PubSubs at the 5 s cap at 200 CLUSTER SLOTS per second).

import (
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
