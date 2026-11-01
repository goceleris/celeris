package redis

// celeris#1204: a cluster PubSub that holds shard channels of two slots.
//
//   - The replay after a reroute sent every shard channel in ONE SSUBSCRIBE (the
//     base) or only the channels of the served slot (#1154). A cluster refuses a
//     command whose keys span slots (-CROSSSLOT), so the first made both
//     channels silently dead; the second leaves a channel out that the new node
//     would serve. The replay now sends one SSUBSCRIBE per slot, the served slot
//     first.
//   - A single SSubscribe call whose channels span slots is refused with
//     ErrCrossSlot, as a cluster would refuse it, instead of being sent and
//     answered with a -CROSSSLOT that pubsub mode drops.
//
// (Found by the review of PR #1154.)

import (
	"errors"
	"slices"
	"testing"
	"time"
)

const (
	slotsChA1 = "{a}x"
	slotsChA2 = "{a}z"
	slotsChB1 = "{b}y"
	slotsChB2 = "{b}w"
)

// After a slot move the new owner serves both slots (it owns both): the replay
// subscribes both, one SSUBSCRIBE per slot, the served slot first.
func TestClusterReplayGroupsShardChannelsBySlot1204(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	slotA, slotB := int(Slot(slotsChA1)), int(Slot(slotsChB1))
	ownerA := cl.owner(slotA)
	if ownerA == cl.owner(slotB) {
		t.Fatal("test setup: the two slots share a node")
	}
	var to *rpNode
	for _, n := range cl.nodes {
		if n != ownerA && n != cl.owner(slotB) {
			to = n
		}
	}
	ps, err := cc.SSubscribe(rpCtx(t), slotsChA1, slotsChA2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return ownerA.subscribers("ssub") == 1 })
	// The second slot lives on another node: answered -MOVED, not served.
	if err := ps.SSubscribe(rpCtx(t), slotsChB1, slotsChB2); err != nil {
		t.Fatal(err)
	}

	cl.moveSlot(slotB, to, 0) // nobody is subscribed to it: no push
	cl.moveSlot(slotA, to, 0) // the served slot: the owner drops the subscription
	poll(t, "the served slot on the new owner", 10*time.Second, func() bool { return to.subscribers("ssub") == 1 })
	// The replay writes its commands back to back.
	poll(t, "one SSUBSCRIBE per slot on the new owner", 2*time.Second, func() bool { return len(to.ssubCommands()) >= 2 })

	want := [][]string{{slotsChA1, slotsChA2}, {slotsChB2, slotsChB1}}
	if got := to.ssubCommands(); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("the new owner received SSUBSCRIBE %q, want one per slot, the served slot first: %q", got, want)
	}
	cl.spublish(slotsChB1, "mb")
	cl.spublish(slotsChA1, "ma")
	poll(t, "a message of the first slot", 5*time.Second, func() bool { return rc.count("ma") == 1 })
	poll(t, "a message of the second slot", 5*time.Second, func() bool { return rc.count("mb") == 1 })
}

// The replay sends a slot other than the served one only to a node that owns
// it, by the topology the reroute just read: a node that does not would answer
// -MOVED (ignored), and one for a slot with no owner -CLUSTERDOWN, which is a
// reroute trigger. The served slot stays up.
func TestClusterReplayDoesNotSendAnotherNodesSlot1204(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	slotA, slotB := int(Slot(slotsChA1)), int(Slot(slotsChB1))
	ownerA, ownerB := cl.owner(slotA), cl.owner(slotB)
	var to *rpNode
	for _, n := range cl.nodes {
		if n != ownerA && n != ownerB {
			to = n
		}
	}
	ps, err := cc.SSubscribe(rpCtx(t), slotsChA1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return ownerA.subscribers("ssub") == 1 })
	if err := ps.SSubscribe(rpCtx(t), slotsChB1); err != nil {
		t.Fatal(err)
	}
	cl.moveSlot(slotA, to, 0)
	poll(t, "the served slot subscribed", 10*time.Second, func() bool { return to.subscribers("ssub") == 1 })
	time.Sleep(300 * time.Millisecond)
	if got := to.ssubCommands(); len(got) != 1 {
		t.Errorf("the new owner received SSUBSCRIBE %q, want only the served slot's (the other slot is another node's)", got)
	}
	cl.spublish(slotsChA1, "ma")
	poll(t, "a message of the served slot", 5*time.Second, func() bool { return rc.count("ma") == 1 })
	n := to.acceptedConns()
	time.Sleep(700 * time.Millisecond)
	if got := to.acceptedConns(); got != n {
		t.Errorf("the PubSub dialled %d more conns", got-n)
	}
}

// A slot the PubSub holds a channel of loses its owner (DELSLOTS with
// cluster-require-full-coverage no): the node answers its SSUBSCRIBE
// -CLUSTERDOWN Hash slot not served. A replay that sends it makes a reroute,
// whose replay sends it again: the PubSub reroutes for ever (r5 of #1210, real
// Redis 8.0.2: 9 attempts in 12 s, main stable). The topology names no owner, so
// the replay leaves the slot out and the PubSub settles after one reroute.
func TestClusterReplayOfAnUnboundSlotDoesNotRerouteForEver1204(t *testing.T) {
	cl := newRPCluster(t, 1)
	cc := newRPClusterClient(t, cl)
	n := cl.nodes[0]
	ps, err := cc.SSubscribe(rpCtx(t), slotsChA1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "SSUBSCRIBE", 5*time.Second, func() bool { return n.subscribers("ssub") == 1 })
	if err := ps.SSubscribe(rpCtx(t), slotsChB1); err != nil { // the same node owns it: accepted
		t.Fatal(err)
	}
	cl.unbind(int(Slot(slotsChB1)))
	ps.mu.Lock()
	conn := ps.conn
	ps.mu.Unlock()
	_ = conn.closeWithErr(errors.New("test: conn killed"))
	poll(t, "the served slot back after the kill", 10*time.Second, func() bool {
		return n.acceptedConns() >= 2 && n.subscribers("ssub") == 1
	})
	time.Sleep(300 * time.Millisecond) // a reroute the unbound slot would start
	c0 := n.acceptedConns()
	time.Sleep(1200 * time.Millisecond)
	if got := n.acceptedConns() - c0; got != 0 {
		t.Errorf("the PubSub dialled %d more conns in 1.2 s after the replay: it reroutes for ever on the unbound slot", got)
	}
	cl.spublish(slotsChA1, "ma")
	poll(t, "a message of the served slot", 5*time.Second, func() bool { return rc.count("ma") == 1 })
}

// One SSubscribe call whose channels span slots is refused the way a cluster
// refuses the command, before anything is sent or tracked.
func TestClusterSSubscribeAcrossSlotsIsCrossSlot1204(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)

	t.Run("constructor", func(t *testing.T) {
		ps, err := cc.SSubscribe(rpCtx(t), slotsChA1, slotsChB1)
		if ps != nil {
			_ = ps.Close()
		}
		if !errors.Is(err, ErrCrossSlot) {
			t.Fatalf("SSubscribe across slots: err = %v, want ErrCrossSlot", err)
		}
		if ps != nil {
			t.Fatal("a PubSub was returned with the error")
		}
		for _, n := range cl.nodes {
			if c := n.subCmds.Load(); c != 0 {
				t.Errorf("node %s received %d subscribe commands, want none", n.addr, c)
			}
		}
		// Refused before a conn was dialled (the command conns of the client
		// are not counted: only a pub/sub dial would be an extra accept).
		base := make([]int, len(cl.nodes))
		for i, n := range cl.nodes {
			base[i] = n.acceptedConns()
		}
		if ps, err := cc.SSubscribe(rpCtx(t), slotsChA1, slotsChB1); err == nil || ps != nil {
			t.Fatalf("second call: ps=%v err=%v", ps, err)
		}
		for i, n := range cl.nodes {
			if got := n.acceptedConns() - base[i]; got != 0 {
				t.Errorf("node %s accepted %d conns for a refused call, want none", n.addr, got)
			}
		}
	})

	t.Run("control call", func(t *testing.T) {
		ownerA := cl.owner(int(Slot(slotsChA1)))
		ps, err := cc.SSubscribe(rpCtx(t), slotsChA1)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ps.Close() })
		poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return ownerA.subscribers("ssub") == 1 })
		sent := len(ownerA.ssubCommands())
		if err := ps.SSubscribe(rpCtx(t), slotsChA2, slotsChB1); !errors.Is(err, ErrCrossSlot) {
			t.Fatalf("SSubscribe across slots: err = %v, want ErrCrossSlot", err)
		}
		ps.mu.Lock()
		tracked := len(ps.shardSubs)
		ps.mu.Unlock()
		if tracked != 1 {
			t.Errorf("%d shard channels tracked after the refused call, want 1: a refused call must change nothing", tracked)
		}
		time.Sleep(100 * time.Millisecond)
		if got := len(ownerA.ssubCommands()); got != sent {
			t.Errorf("the owner received %d SSUBSCRIBE commands after the refused call", got-sent)
		}
		// Several channels of one slot are fine.
		if err := ps.SSubscribe(rpCtx(t), slotsChA2, "{a}q"); err != nil {
			t.Fatalf("SSubscribe of two channels of one slot: %v", err)
		}
	})
}

// A SUnsubscribe across slots is sent as one SUNSUBSCRIBE per slot: a cluster
// refuses it as one command (-CROSSSLOT, checked on Redis 8.0.2), and the reply
// would be dropped, leaving the server delivering channels the caller removed.
func TestClusterSUnsubscribeAcrossSlotsSendsOneCommandPerSlot1204(t *testing.T) {
	cl := newRPCluster(t, 1) // one node owns every slot, so it accepts both
	cc := newRPClusterClient(t, cl)
	n := cl.nodes[0]
	ps, err := cc.SSubscribe(rpCtx(t), slotsChA1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	if err := ps.SSubscribe(rpCtx(t), slotsChB1); err != nil {
		t.Fatal(err)
	}
	poll(t, "both channels subscribed", 5*time.Second, func() bool {
		return cl.spublish(slotsChA1, "x1") == 1 && cl.spublish(slotsChB1, "x2") == 1
	})
	if err := ps.SUnsubscribe(rpCtx(t), slotsChA1, slotsChB1); err != nil {
		t.Fatalf("SUnsubscribe across slots: %v", err)
	}
	poll(t, "the server holds no shard channel", 2*time.Second, func() bool { return n.subscribers("ssub") == 0 })
	ps.mu.Lock()
	tracked := len(ps.shardSubs)
	ps.mu.Unlock()
	if tracked != 0 {
		t.Errorf("%d shard channels still tracked", tracked)
	}
	if got := cl.spublish(slotsChA1, "y1") + cl.spublish(slotsChB1, "y2"); got != 0 {
		t.Errorf("the server still delivers %d messages of removed channels", got)
	}
	_ = rc
}

// A PubSub that is not routed by slots (a plain Client: a standalone server has
// no slots) is not restricted: its replay is still one command.
func TestStandalonePubSubSSubscribeAcrossSlotsIsNotRefused1204(t *testing.T) {
	cl := newRPCluster(t, 1) // one node owns everything: it accepts any slot
	c, err := NewClient(cl.nodes[0].addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ps, err := c.Subscribe(rpCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	// (The fake answers the command with -CROSSSLOT, as a cluster node would;
	// the reply is not tracked. The point is that the client sends it.)
	if err := ps.SSubscribe(rpCtx(t), slotsChA1, slotsChB1); err != nil {
		t.Fatalf("SSubscribe on a PubSub with no slot routing: %v", err)
	}
	poll(t, "the command at the server", 5*time.Second, func() bool { return len(cl.nodes[0].ssubCommands()) == 1 })
}
