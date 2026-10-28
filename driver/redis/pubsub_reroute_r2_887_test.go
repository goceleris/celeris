package redis

// Review round 2 of celeris#887 item 31 (PR #1154): the cells compile on the
// round-1 head and fail there (N2, N5), or kill mutants that survived its
// suite (N6: every earlier cell had a first channel that also sorts first).

import (
	"testing"
	"time"
)

// N2. A channel of a second slot that the SAME node also owns is accepted by the
// node; when that slot moves away the node sends "sunsubscribe" for it. It is
// not the slot the PubSub serves, so it must not rebuild the served
// subscription (a gap on every served channel).
func TestClusterSunsubscribeOfANonServedSlotDoesNotRerouteTheServedOne887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const a, f = "{a}x", "foo" // slots 15495 and 12182: both on nodes[2]
	sa, sf := int(Slot(a)), int(Slot(f))
	holder := cl.owner(sa)
	if holder != cl.owner(sf) {
		t.Fatalf("setup: slots %d and %d are on different nodes", sa, sf)
	}
	ps, err := cc.SSubscribe(rpCtx(t), a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return holder.subscribers("ssub") == 1 })
	if err := ps.SSubscribe(rpCtx(t), f); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	cl.spublish(f, "f1")
	poll(t, "f1 (the second slot is accepted by the same node)", 3*time.Second, func() bool { return rc.count("f1") == 1 })
	accepted := holder.acceptedConns()

	cl.moveSlot(sf, cl.nodes[0], 0) // the NON-served slot moves away
	time.Sleep(1200 * time.Millisecond)
	if n := holder.acceptedConns() - accepted; n != 0 {
		t.Errorf("the move of a slot the PubSub does not serve made it dial %d more conns: the served subscription was rebuilt (a gap on %s)", n, a)
	}
	cl.spublish(a, "a1")
	poll(t, "a1", 3*time.Second, func() bool { return rc.count("a1") == 1 })
}

// N6. The served slot is the slot of the FIRST channel, even when a later
// channel of another slot sorts lower.
func TestClusterFirstChannelNotSmallestStaysServed887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const z, a = "{z}q", "{a}x"
	if cl.owner(int(Slot(z))) == cl.owner(int(Slot(a))) {
		t.Fatalf("setup: same node (%d, %d)", Slot(z), Slot(a))
	}
	ps, err := cc.SSubscribe(rpCtx(t), z)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return cl.owner(int(Slot(z))).subscribers("ssub") == 1 })
	_ = ps.SSubscribe(rpCtx(t), a) // -MOVED for a's slot: ignored
	time.Sleep(1200 * time.Millisecond)
	cl.spublish(z, "z1")
	poll(t, "z1 on the first channel", 3*time.Second, func() bool { return rc.count("z1") == 1 })
}

// N6. The reroute follows the served slot even when a channel of another slot
// sorts lower: a pick by the smallest channel of any slot would chase the other
// slot's owner, get -MOVED for the served slot and loop for ever.
func TestClusterRerouteFollowsTheServedSlotNotTheSmallestChannel887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const z, a = "{z}q", "{a}x"
	sz := int(Slot(z))
	oz, oa := cl.owner(sz), cl.owner(int(Slot(a)))
	if oz == oa {
		t.Fatal("setup: same node")
	}
	var third *rpNode
	for _, n := range cl.nodes {
		if n != oz && n != oa {
			third = n
		}
	}
	ps, err := cc.SSubscribe(rpCtx(t), z)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return oz.subscribers("ssub") == 1 })
	_ = ps.SSubscribe(rpCtx(t), a)
	time.Sleep(300 * time.Millisecond)
	cl.moveSlot(sz, third, 0)
	poll(t, "the served channel on the new owner", 10*time.Second, func() bool { return third.subscribers("ssub") == 1 })
	cl.spublish(z, "z1")
	poll(t, "z1", 3*time.Second, func() bool { return rc.count("z1") == 1 })
}

// N5. The failing mark the command path sets after a PubSub dial failed is the
// command path's: a later successful PubSub dial must not clear it (the
// PubSub's own mark went stale when the command path took the node over).
func TestPubSubDialSuccessDoesNotClearAMarkTheCommandPathSetAfterwards887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	route := clusterPubSubRoute{cc}
	cc.mu.RLock()
	var n *clusterNode
	for _, nd := range cc.nodes {
		n = nd
		break
	}
	cc.mu.RUnlock()
	route.dialed(n.client, false) // the PubSub path's verdict
	n.failing.Store(false)        // a topology probe of this node found it alive
	n.failing.Store(true)         // a command hit a network error
	route.dialed(n.client, true)  // a PubSub dial succeeded
	if !n.failing.Load() {
		t.Error("a successful PubSub dial cleared the failing mark the command path set after the PubSub's own")
	}
}
