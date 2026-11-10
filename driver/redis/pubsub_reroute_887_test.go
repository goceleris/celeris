package redis

// celeris#887 item 31 (decision D3, option A): a cluster PubSub follows its
// subscription. SUBSCRIBE is node-local, so when the node holding it goes away
// the PubSub subscribes again on another node; a sharded subscription follows
// its slot (a gone owner, a slot that moved: the server's unsolicited
// "sunsubscribe", a -MOVED answer to the re-subscribe). The caller keeps
// reading the same Channel(); a message published while the PubSub switches is
// lost (a gap), never delivered twice.

import (
	"context"
	"testing"
	"time"
)

func newRPClusterClient(t *testing.T, cl *rpCluster) *ClusterClient {
	t.Helper()
	cc, err := NewClusterClient(ClusterConfig{Addrs: cl.addrs(), DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

func rpCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A regular SUBSCRIBE lives on one node. When that node goes away the PubSub
// subscribes on another one and keeps delivering to the same channel.
func TestClusterSubscribeFollowsALostNode887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	ps, err := cc.Subscribe(rpCtx(t), "news")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "the subscription on a node", 5*time.Second, func() bool { return cl.holder("sub") != nil })
	first := cl.holder("sub")
	cl.publish("news", "m1")
	poll(t, "m1", 5*time.Second, func() bool { return rc.count("m1") == 1 })

	first.kill()
	poll(t, "the subscription on another node", 10*time.Second, func() bool {
		h := cl.holder("sub")
		return h != nil && h != first
	})
	cl.publish("news", "m2")
	poll(t, "m2 on the same channel", 5*time.Second, func() bool { return rc.count("m2") >= 1 })
	time.Sleep(100 * time.Millisecond)
	if n := rc.count("m2"); n != 1 {
		t.Errorf("m2 delivered %d times, want exactly 1", n)
	}
	if n := cl.subscribers("sub"); n != 1 {
		t.Errorf("%d conns hold the subscription, want 1", n)
	}
}

// A node that is down when Subscribe is called does not fail the call: any
// live node will do for a regular SUBSCRIBE.
func TestClusterSubscribeSkipsADeadNode887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	cl.nodes[0].kill()
	for i := 0; i < 15; i++ {
		ps, err := cc.Subscribe(rpCtx(t), "news")
		if err != nil {
			t.Fatalf("call %d: Subscribe: %v", i, err)
		}
		_ = ps.Close()
	}
}

// A sharded subscription follows its slot when the slot moves: the old owner
// sends "sunsubscribe" and keeps the conn, and the PubSub subscribes on the new
// owner. The old conn is released.
func TestClusterSSubscribeFollowsASlotMove887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const ch = "foo" // slot 12182
	slot := int(Slot(ch))
	from := cl.owner(slot)
	to := cl.nodes[0]
	if from == to {
		t.Fatal("test setup: slot already on the target")
	}
	ps, err := cc.SSubscribe(rpCtx(t), ch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return from.subscribers("ssub") == 1 })
	cl.spublish(ch, "m1")
	poll(t, "m1", 5*time.Second, func() bool { return rc.count("m1") == 1 })

	cl.moveSlot(slot, to, 0)
	poll(t, "SSUBSCRIBE on the new owner", 10*time.Second, func() bool { return to.subscribers("ssub") == 1 })
	cl.spublish(ch, "m2")
	poll(t, "m2 on the same channel", 5*time.Second, func() bool { return rc.count("m2") == 1 })
	poll(t, "the conn to the old owner released", 5*time.Second, func() bool {
		return from.conn(func(*rpConn) bool { return true }) == nil || from.subscribers("ssub") == 0
	})
	if n := cl.subscribers("ssub"); n != 1 {
		t.Errorf("%d conns hold the shard subscription, want 1", n)
	}
}

// The owner of a slot goes away and a replica takes its slots a little later:
// until then CLUSTER SLOTS still names the dead node. The PubSub keeps trying
// with backoff and ends on the new owner.
func TestClusterSSubscribeFollowsAFailover887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const ch = "foo"
	from := cl.owner(int(Slot(ch)))
	to := cl.nodes[0]
	ps, err := cc.SSubscribe(rpCtx(t), ch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return from.subscribers("ssub") == 1 })

	from.kill()
	time.Sleep(400 * time.Millisecond)
	cl.failover(from, to, 0)
	poll(t, "SSUBSCRIBE on the new owner", 15*time.Second, func() bool { return to.subscribers("ssub") == 1 })
	cl.spublish(ch, "m1")
	poll(t, "m1", 5*time.Second, func() bool { return rc.count("m1") == 1 })
}

// The slot moves but the nodes still answer CLUSTER SLOTS with the old table
// for a while. The re-subscribe goes to the old owner and is answered -MOVED,
// which used to be dropped in pubsub mode (the subscription was dead and
// nothing said so). The MOVED reroutes again, with backoff, until the table
// catches up.
func TestClusterSSubscribeMovedAnswerReroutes887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const ch = "foo"
	slot := int(Slot(ch))
	from := cl.owner(slot)
	to := cl.nodes[0]
	ps, err := cc.SSubscribe(rpCtx(t), ch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return from.subscribers("ssub") == 1 })

	cl.moveSlot(slot, to, 3)
	poll(t, "SSUBSCRIBE on the new owner", 15*time.Second, func() bool { return to.subscribers("ssub") == 1 })
	cl.spublish(ch, "m1")
	poll(t, "m1", 5*time.Second, func() bool { return rc.count("m1") == 1 })
	if got := from.subCmds.Load(); got < 2 {
		t.Errorf("the old owner saw %d SSUBSCRIBE commands, want the replay to have hit it (stale table) too", got)
	}
}

// Two PubSubs on different nodes, one of the nodes goes away: only that one
// moves. The other keeps its conn and gets every message once.
func TestClusterTwoSubscriptionsOneLost887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	var ps1, ps2 *PubSub
	var n1, n2 *rpNode
	for try := 0; try < 40; try++ {
		a, err := cc.Subscribe(rpCtx(t), "one")
		if err != nil {
			t.Fatal(err)
		}
		poll(t, "first subscription", 5*time.Second, func() bool { return cl.subscribers("sub") == 1 })
		h1 := cl.holder("sub")
		b, err := cc.Subscribe(rpCtx(t), "two")
		if err != nil {
			t.Fatal(err)
		}
		poll(t, "second subscription", 5*time.Second, func() bool { return cl.subscribers("sub") == 2 })
		var h2 *rpNode
		for _, nd := range cl.nodes {
			if nd != h1 && nd.subscribers("sub") == 1 {
				h2 = nd
			}
		}
		if h2 != nil {
			ps1, ps2, n1, n2 = a, b, h1, h2
			break
		}
		_ = a.Close()
		_ = b.Close()
		poll(t, "both closed", 5*time.Second, func() bool { return cl.subscribers("sub") == 0 })
	}
	if ps1 == nil {
		t.Fatal("could not place the two subscriptions on different nodes")
	}
	t.Cleanup(func() { _ = ps1.Close(); _ = ps2.Close() })
	rc1, rc2 := collect(ps1), collect(ps2)
	conn2 := n2.conn(func(c *rpConn) bool { c.mu.Lock(); defer c.mu.Unlock(); return len(c.subs) > 0 })

	n1.kill()
	poll(t, "the first subscription on a live node", 10*time.Second, func() bool {
		h := cl.holder("sub")
		return cl.subscribers("sub") == 2 && h != nil && h != n1
	})
	cl.publish("one", "a")
	cl.publish("two", "b")
	poll(t, "both messages", 5*time.Second, func() bool { return rc1.count("a") >= 1 && rc2.count("b") >= 1 })
	time.Sleep(100 * time.Millisecond)
	if rc1.count("a") != 1 || rc2.count("b") != 1 {
		t.Errorf("deliveries: one/a=%d two/b=%d, want 1 and 1", rc1.count("a"), rc2.count("b"))
	}
	conn2.mu.Lock()
	still := len(conn2.subs)
	conn2.mu.Unlock()
	if still != 1 {
		t.Errorf("the subscription that was not lost was disturbed (its conn has %d subscriptions)", still)
	}
}

// A server that moves the slot away again at every SSUBSCRIBE: the PubSub
// keeps trying, but follows the existing backoff (50 ms, 100 ms, ... doubling),
// it does not spin.
func TestClusterSSubscribeFlappingStaysInsideTheBackoff887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const ch = "foo"
	owner := cl.owner(int(Slot(ch)))
	var subs func() int64
	owner.setOnSSub(func(n *rpNode, c *rpConn, ch string) {
		// The slot "moves" right after every subscription: the owner takes
		// the subscription back with a sunsubscribe, and stays the owner.
		n.sunsubscribeSlot(int(Slot(ch)))
	})
	subs = owner.subCmds.Load
	ps, err := cc.SSubscribe(rpCtx(t), ch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	time.Sleep(2500 * time.Millisecond)
	n := subs()
	t.Logf("%d SSUBSCRIBE commands in 2.5 s", n)
	// 50+100+200+400+800+1600 ms of delay: about 7 attempts. A loop that
	// starts over from the base delay would make about 40.
	if n > 12 {
		t.Errorf("%d SSUBSCRIBEs in 2.5 s: the reroute did not back off", n)
	}
	if n < 3 {
		t.Errorf("only %d SSUBSCRIBEs in 2.5 s: the unsolicited sunsubscribe did not make the PubSub subscribe again", n)
	}
}

// The old conn is cut off from the PubSub before the new conn is bound, so a
// message the cluster delivers on both while the PubSub switches reaches the
// caller once. Here the new node publishes a probe on a channel the old conn
// is still subscribed to, right after it acks the replay.
func TestClusterRerouteDeliversASwitchWindowMessageOnce887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const ch = "foo"
	slot := int(Slot(ch))
	from := cl.owner(slot)
	to := cl.nodes[0]
	ps, err := cc.SSubscribe(rpCtx(t), ch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	if err := ps.Subscribe(rpCtx(t), "x"); err != nil {
		t.Fatal(err)
	}
	rc := collect(ps)
	poll(t, "both subscriptions on the owner", 5*time.Second, func() bool {
		return from.subscribers("ssub") == 1 && from.subscribers("sub") == 1
	})
	to.setAfterSub(func(n *rpNode, c *rpConn, verb string) {
		if verb == "SUBSCRIBE" {
			cl.publish("x", "probe") // reaches the old conn (still open) and the new one
		}
	})
	cl.moveSlot(slot, to, 0)
	poll(t, "the probe", 10*time.Second, func() bool { return rc.count("probe") >= 1 })
	time.Sleep(300 * time.Millisecond)
	if n := rc.count("probe"); n != 1 {
		t.Errorf("the probe was delivered %d times, want 1 (a gap is allowed, a duplicate is not)", n)
	}
}

// A conn that stalls its HELLO (a node that accepts and says nothing) must not
// keep the reroute goroutine alive after Close: the dial is cut short.
func TestClusterPubSubCloseCutsAStalledRerouteDial887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	ps, err := cc.Subscribe(rpCtx(t), "news")
	if err != nil {
		t.Fatal(err)
	}
	poll(t, "the subscription", 5*time.Second, func() bool { return cl.holder("sub") != nil })
	first := cl.holder("sub")
	for _, nd := range cl.nodes {
		if nd != first {
			nd.mu.Lock()
			nd.hang = true
			nd.mu.Unlock()
		}
	}
	first.kill()
	// The reroute dials the other nodes, which never answer.
	time.Sleep(600 * time.Millisecond)
	start := time.Now()
	_ = ps.Close()
	if d := time.Since(start); d > time.Second {
		t.Errorf("Close took %v", d)
	}
	poll(t, "the reroute loop to end", 2*time.Second, func() bool { return rpLoopIdle(ps) })
}

// Close races a reroute at every point of it, and the conns are all released.
func TestClusterPubSubCloseRacesAReroute887(t *testing.T) {
	for i := 0; i < 60; i++ {
		cl := newRPCluster(t, 3)
		cc, err := NewClusterClient(ClusterConfig{Addrs: cl.addrs(), DialTimeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		ps, err := cc.Subscribe(rpCtx(t), "news")
		if err != nil {
			t.Fatal(err)
		}
		poll(t, "the subscription", 5*time.Second, func() bool { return cl.holder("sub") != nil })
		first := cl.holder("sub")
		first.kill()
		time.Sleep(time.Duration(i) * 4 * time.Millisecond) // 0 .. 240 ms: before, between and after the attempts
		_ = ps.Close()
		_ = cc.Close()
		for _, nd := range cl.nodes {
			nd.kill()
		}
		poll(t, "no reroute loop", 3*time.Second, func() bool { return rpLoopIdle(ps) })
	}
}

// A PubSub that is not closed by its owner is closed with the ClusterClient: the
// reroute has nowhere to go, the loop gives up, and Channel() is closed.
func TestClusterCloseEndsThePubSubReroute887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	ps, err := cc.Subscribe(rpCtx(t), "news")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "the subscription", 5*time.Second, func() bool { return cl.holder("sub") != nil })
	cl.holder("sub").kill()
	_ = cc.Close()
	poll(t, "Channel() closed", 5*time.Second, func() bool { return rc.closed.Load() })
	poll(t, "no reroute loop", 2*time.Second, func() bool { return rpLoopIdle(ps) })
}
