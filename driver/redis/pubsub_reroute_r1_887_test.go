package redis

// Review round 1 of celeris#887 item 31 (PR #1154): the cells below compile on
// the first head and fail there (F2, F2b, F3, F4, F7), or pin a gap the first
// head's tests had (F1: a mutant survived).

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// F1. The first head's guard for the closed check before the install used a
// PubSub with a channel, whose replay (it checks closed before every write)
// released the conn anyway. With no channel the replay writes nothing, so only
// the check before the install keeps a conn off a closed PubSub.
func TestClusterRerouteDoesNotInstallOnAClosedPubSubWithNoChannels887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	ps, err := cc.Subscribe(rpCtx(t)) // no channels: the replay writes nothing
	if err != nil {
		t.Fatal(err)
	}
	ps.mu.Lock()
	home := ps.connClient
	ps.mu.Unlock()
	var homeAddr string
	cc.mu.RLock()
	for a, n := range cc.nodes {
		if n.client == home {
			homeAddr = a
		}
	}
	cc.mu.RUnlock()
	reached, proceed := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	hook := func(p *PubSub) {
		if p != ps || !once.CompareAndSwap(false, true) {
			return
		}
		close(reached)
		<-proceed
	}
	testHookBeforeInstall.Store(&hook)
	t.Cleanup(func() { testHookBeforeInstall.Store(nil) })
	for _, n := range cl.nodes {
		if n.addr == homeAddr {
			n.kill()
		}
	}
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the reroute loop never reached the install")
	}
	_ = ps.Close()
	close(proceed)
	poll(t, "the reroute loop to end", 2*time.Second, func() bool { return rpLoopIdle(ps) })
	ps.mu.Lock()
	conn := ps.conn
	ps.mu.Unlock()
	if conn != nil {
		t.Errorf("a conn was installed on a closed PubSub (nobody releases it)")
	}
	cc.mu.RLock()
	var pools []*redisPool
	for _, nd := range cc.nodes {
		pools = append(pools, nd.client.pool)
	}
	cc.mu.RUnlock()
	poll(t, "every pubsub pool back to zero open conns", 3*time.Second, func() bool {
		for _, pl := range pools {
			if pl.pubsub.Stats().Open != 0 {
				return false
			}
		}
		return true
	})
}

// F2. A dial that fails because the CALLER's context ended says nothing about
// the node: it must not be marked failing (anyNode steers Publish, keyless
// commands and the topology probe away from failing nodes).
func TestClusterSubscribeWithACancelledContextDoesNotMarkANodeFailing887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = cc.Subscribe(ctx, "x")
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	for addr, n := range cc.nodes {
		if n.failing.Load() {
			t.Errorf("healthy node %s marked failing by a Subscribe whose context was cancelled", addr)
		}
	}
}

// F2b. A PubSub dial that succeeds clears only the failing mark a PubSub dial
// set; one the command path set (a network error on a command) is the command
// path's to clear.
func TestPubSubDialReportClearsOnlyTheMarkItSet887(t *testing.T) {
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

	n.failing.Store(true) // the command path's verdict
	route.dialed(n.client, true)
	if !n.failing.Load() {
		t.Error("a successful PubSub dial cleared the failing mark the command path set")
	}
	n.failing.Store(false)

	route.dialed(n.client, false) // the PubSub path's own verdict ...
	if !n.failing.Load() {
		t.Fatal("a failed PubSub dial did not mark the node failing (the next attempt would not move on)")
	}
	route.dialed(n.client, true) // ... and its own recovery
	if n.failing.Load() {
		t.Error("a successful PubSub dial did not clear the mark a failed PubSub dial set")
	}
}

// F3. Real Redis answers SUNSUBSCRIBE of a channel it holds no subscription for,
// in a slot it does not serve, with -MOVED. That is an answer to the caller's
// harmless call, not a sign that the subscription is lost: it must not rebuild
// a healthy subscription (a delivery gap for every channel of the PubSub).
func TestClusterForeignSUnsubscribeDoesNotRerouteAHealthySubscription887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const a, foreign = "{a}x", "{b}never" // slots 15495 and 3300: different nodes
	if cl.owner(int(Slot(a))) == cl.owner(int(Slot(foreign))) {
		t.Fatal("test setup: the two slots share a node")
	}
	ps, err := cc.SSubscribe(rpCtx(t), a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	holder := cl.owner(int(Slot(a)))
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return holder.subscribers("ssub") == 1 })
	accepted, cmds := holder.acceptedConns(), holder.subCmds.Load()

	if err := ps.SUnsubscribe(rpCtx(t), foreign); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	if n := holder.acceptedConns(); n != accepted {
		t.Errorf("the PubSub dialled %d more conns after a SUnsubscribe of a channel it never had: a healthy subscription was rebuilt", n-accepted)
	}
	if n := holder.subCmds.Load(); n != cmds {
		t.Errorf("the owner saw %d more SSUBSCRIBEs", n-cmds)
	}
	cl.spublish(a, "m1")
	poll(t, "m1", 5*time.Second, func() bool { return rc.count("m1") == 1 })
}

// F4. Two slots on one PubSub, the second on another node: the channel of
// the first slot keeps working, as on the base. The first head sent both in one
// SSUBSCRIBE after a reroute, got -CROSSSLOT, and left both dead.
func TestClusterSecondSlotOnOnePubSubDoesNotKillTheFirst887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const a, b = "{a}x", "{b}y"
	slotA := int(Slot(a))
	from := cl.owner(slotA)
	if from == cl.owner(int(Slot(b))) {
		t.Fatal("test setup: the two slots share a node")
	}
	ps, err := cc.SSubscribe(rpCtx(t), a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return from.subscribers("ssub") == 1 })
	if err := ps.SSubscribe(rpCtx(t), b); err != nil { // answered -MOVED: b is not served
		t.Fatal(err)
	}
	time.Sleep(900 * time.Millisecond) // a reroute (and its replay) would have happened by now
	cl.spublish(a, "m1")
	poll(t, "m1 on the first channel", 5*time.Second, func() bool { return rc.count("m1") == 1 })

	// The same after the first channel's slot moves: the reroute replays the
	// channel of the served slot, not both.
	to := cl.nodes[1]
	if to == from || to == cl.owner(int(Slot(b))) {
		to = cl.nodes[0]
	}
	cl.moveSlot(slotA, to, 0)
	poll(t, "SSUBSCRIBE of the first channel on the new owner", 10*time.Second, func() bool { return to.subscribers("ssub") == 1 })
	cl.spublish(a, "m2")
	poll(t, "m2 on the first channel", 5*time.Second, func() bool { return rc.count("m2") == 1 })
}

// F7. A dead owner that the (still stale) topology keeps naming is dialled once
// by the constructor, not once per node of the cluster, each with a topology
// refresh in front of it.
func TestClusterSSubscribeTriesADeadOwnerOnce887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const ch = "foo"
	cl.owner(int(Slot(ch))).kill()
	before := cl.slotsReplies.Load()
	if _, err := cc.SSubscribe(rpCtx(t), ch); err == nil {
		t.Fatal("SSubscribe on a dead owner returned no error")
	}
	if n := cl.slotsReplies.Load() - before; n > 1 {
		t.Errorf("the constructor refreshed the topology %d times for one dead owner (it names the same node every time), want at most 1", n)
	}
}
