package redis

// More cells of celeris#887 item 31: what is and is not a trigger, a trigger
// that arrives during the handover, a control call racing a reroute, RESP2, and
// the sentinel's master PubSub.

import (
	"context"
	"testing"
	"time"
)

// A standalone PubSub (no route) does not act on any of it: a sunsubscribe is
// the caller's business, as before.
func TestStandalonePubSubIgnoresSunsubscribe887(t *testing.T) {
	cl := newRPCluster(t, 1)
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
	_ = ps.SSubscribe(rpCtx(t), "foo")
	node := cl.nodes[0]
	poll(t, "the SSUBSCRIBE", 5*time.Second, func() bool { return node.subscribers("ssub") == 1 })
	node.sunsubscribeSlot(int(Slot("foo")))
	time.Sleep(400 * time.Millisecond)
	if n := node.subCmds.Load(); n != 1 {
		t.Errorf("a standalone PubSub subscribed again (%d SSUBSCRIBEs)", n)
	}
}

// A control call racing a reroute is neither lost nor undone: whatever the
// order, the channel the caller removed is not subscribed again anywhere, and
// the one it added is.
func TestClusterControlCallRacingARerouteLeavesTheCallerWithItsSet887(t *testing.T) {
	for i := 0; i < 20; i++ {
		cl := newRPCluster(t, 3)
		cc, err := NewClusterClient(ClusterConfig{Addrs: cl.addrs(), DialTimeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		const ch, ch2 = "{foo}1", "{foo}2" // same slot
		slot := int(Slot(ch))
		from := cl.owner(slot)
		to := cl.nodes[0]
		if from == to {
			to = cl.nodes[1]
		}
		ps, err := cc.SSubscribe(rpCtx(t), ch)
		if err != nil {
			t.Fatal(err)
		}
		poll(t, "SSUBSCRIBE", 5*time.Second, func() bool { return from.subscribers("ssub") == 1 })
		done := make(chan struct{})
		go func() {
			defer close(done)
			time.Sleep(time.Duration(i) * 200 * time.Microsecond)
			_ = ps.SSubscribe(context.Background(), ch2)
			_ = ps.SUnsubscribe(context.Background(), ch)
		}()
		cl.moveSlot(slot, to, 0)
		<-done
		// Settled: only ch2 is subscribed, on the new owner.
		poll(t, "ch2 on the new owner, ch gone", 10*time.Second, func() bool {
			c := to.conn(func(c *rpConn) bool {
				c.mu.Lock()
				defer c.mu.Unlock()
				_, a := c.ssubs[ch2]
				_, b := c.ssubs[ch]
				return a && !b
			})
			return c != nil && cl.subscribers("ssub") == 1
		})
		_ = ps.Close()
		_ = cc.Close()
	}
}

// Closing the PubSub at every point of a reroute releases every conn: no
// server-side conn is left behind (the pool's counts are not touched either).
func TestClusterPubSubCloseLeavesNoConnBehind887(t *testing.T) {
	for i := 0; i < 40; i++ {
		cl := newRPCluster(t, 3)
		cc, err := NewClusterClient(ClusterConfig{Addrs: cl.addrs(), DialTimeout: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		const ch = "foo"
		slot := int(Slot(ch))
		to := cl.nodes[0]
		ps, err := cc.SSubscribe(rpCtx(t), ch)
		if err != nil {
			// (Seen once, on Windows, as "no cluster nodes available": say what
			// the client's table looked like.)
			cc.mu.RLock()
			nodes, owner := len(cc.nodes), cc.slots[slot] != nil
			cc.mu.RUnlock()
			t.Fatalf("iteration %d: SSubscribe: %v (the client knows %d nodes, slot owner known: %v, CLUSTER SLOTS replies given: %d)",
				i, err, nodes, owner, cl.slotsReplies.Load())
		}
		poll(t, "SSUBSCRIBE", 5*time.Second, func() bool { return cl.subscribers("ssub") == 1 })
		cl.moveSlot(slot, to, 0)
		poll(t, "SSUBSCRIBE on the second node", 5*time.Second, func() bool { return to.subscribers("ssub") == 1 })
		// A second move: the conn the loop releases now came from a node that
		// is not the one the PubSub started on.
		third := cl.nodes[1]
		if third == to || third == cl.owner(slot) {
			third = cl.nodes[2]
		}
		cl.moveSlot(slot, third, 0)
		time.Sleep(time.Duration(i) * 3 * time.Millisecond)
		_ = ps.Close()
		// Every conn went back to the pool it came from: no node's pubsub pool
		// counts one still open, and none counts one too few (a conn released
		// to the pool of the client the PubSub started on would do both).
		cc.mu.RLock()
		pools := map[string]*redisPool{}
		for addr, n := range cc.nodes {
			pools[addr] = n.client.pool
		}
		cc.mu.RUnlock()
		// (Close does not wait for a dial of the loop that is in flight: it
		// releases its conn a moment later. A conn that is never released, or
		// released to the wrong pool, does not settle.)
		poll(t, "every pubsub pool back to zero open conns", 5*time.Second, func() bool {
			for _, pl := range pools {
				if pl.pubsub.Stats().Open != 0 {
					return false
				}
			}
			return true
		})
		_ = cc.Close()
		poll(t, "every conn closed", 5*time.Second, func() bool { return cl.liveConns() == 0 })
	}
}

// A SentinelClient's PubSub follows a failover: it subscribes on the new master
// and keeps delivering to the same channel. Before, it retried the old master's
// closed client for ever.
func TestSentinelPubSubFollowsAFailover887(t *testing.T) {
	cl := newRPCluster(t, 2)
	m1, m2 := cl.nodes[0], cl.nodes[1]
	sentinel := startFakeSentinel(t, m1.addr)
	sc, err := NewSentinelClient(SentinelConfig{
		MasterName:    "mymaster",
		SentinelAddrs: []string{sentinel.Addr()},
		DialTimeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	ps, err := sc.Subscribe(rpCtx(t), "news")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "the subscription on the master", 5*time.Second, func() bool { return m1.subscribers("sub") == 1 })

	m1.kill()
	sentinel.SetMaster(m2.addr)
	poll(t, "the subscription on the new master", 15*time.Second, func() bool { return m2.subscribers("sub") == 1 })
	cl.publish("news", "m1")
	poll(t, "m1 on the same channel", 5*time.Second, func() bool { return rc.count("m1") == 1 })
}

// Closing the SentinelClient ends its PubSubs' reroute: Channel() closes.
func TestSentinelCloseEndsThePubSubReroute887(t *testing.T) {
	cl := newRPCluster(t, 1)
	sentinel := startFakeSentinel(t, cl.nodes[0].addr)
	sc, err := NewSentinelClient(SentinelConfig{
		MasterName:    "mymaster",
		SentinelAddrs: []string{sentinel.Addr()},
		DialTimeout:   2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := sc.Subscribe(rpCtx(t), "news")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "the subscription", 5*time.Second, func() bool { return cl.nodes[0].subscribers("sub") == 1 })
	_ = sc.Close()
	poll(t, "Channel() closed", 5*time.Second, func() bool { return rc.closed.Load() })
	poll(t, "no reroute loop", 2*time.Second, func() bool { return rpLoopIdle(ps) })
}

// Two unsolicited sunsubscribes of the same move (one per channel of the slot)
// make one reroute, not two: the second arrives while the loop is running, from
// the old conn, and the loop's replay already answers it. A flag kept from
// before the install would start another loop (another gap, another dial).
func TestClusterRerouteAnswersEveryChannelOfAMoveWithOneLoop887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const ch1, ch2 = "{foo}1", "{foo}2"
	slot := int(Slot(ch1))
	from := cl.owner(slot)
	to := cl.nodes[0]
	if from == to {
		to = cl.nodes[1]
	}
	ps, err := cc.SSubscribe(rpCtx(t), ch1, ch2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	poll(t, "both channels on the owner", 5*time.Second, func() bool { return from.subscribers("ssub") == 1 })
	cl.moveSlot(slot, to, 0)
	poll(t, "SSUBSCRIBE on the new owner", 10*time.Second, func() bool { return to.subscribers("ssub") == 1 })
	// A needless second loop would dial again after its (longer) backoff.
	time.Sleep(700 * time.Millisecond)
	if n := to.subCmds.Load(); n != 1 {
		t.Errorf("the new owner saw %d SSUBSCRIBE commands, want 1: the second sunsubscribe of the move made another reroute", n)
	}
}
