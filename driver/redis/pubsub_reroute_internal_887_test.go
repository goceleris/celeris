package redis

// Cells of celeris#887 item 31 that need the PubSub's unexported route and
// hooks: what is and is not a trigger (RESP3 and RESP2), and a trigger that
// arrives during the handover.

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// sameClientRoute is a route that always answers with the same client: it
// makes a PubSub of a plain Client behave like a cluster PubSub for the
// triggers, so the cells below run on one fake node.
type sameClientRoute struct{ c *Client }

func (r sameClientRoute) pick(context.Context, string, *Client, bool, time.Time) (*Client, error) {
	return r.c, nil
}
func (sameClientRoute) dialed(*Client, bool)      {}
func (sameClientRoute) owns(*Client, uint16) bool { return true }
func (sameClientRoute) slotted() bool             { return true }

// routedPubSub opens a RESP2 PubSub on the only node of cl with a route.
func routedPubSub(t *testing.T, cl *rpCluster, resp2 bool) *PubSub {
	t.Helper()
	var opts []Option
	if resp2 {
		opts = append(opts, WithForceRESP2())
	}
	c, err := NewClient(cl.nodes[0].addr, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ps, err := c.newPubSubRouted(rpCtx(t), sameClientRoute{c})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	return ps
}

// An unsolicited sunsubscribe of a channel the PubSub still wants makes it
// subscribe again, on RESP3 pushes and on RESP2 arrays alike.
func TestPubSubSunsubscribeOfAWantedChannelResubscribes887(t *testing.T) {
	for _, resp2 := range []bool{false, true} {
		name := "resp3"
		if resp2 {
			name = "resp2"
		}
		t.Run(name, func(t *testing.T) {
			cl := newRPCluster(t, 1)
			ps := routedPubSub(t, cl, resp2)
			if err := ps.SSubscribe(rpCtx(t), "foo"); err != nil {
				t.Fatal(err)
			}
			node := cl.nodes[0]
			// (The fake counts a command when it receives it, before it records the
			// subscription: wait for both, or the sunsubscribe below finds nothing.)
			poll(t, "the first SSUBSCRIBE", 5*time.Second, func() bool { return node.subCmds.Load() == 1 && node.subscribers("ssub") == 1 })
			node.sunsubscribeSlot(int(Slot("foo")))
			poll(t, "the second SSUBSCRIBE", 5*time.Second, func() bool { return node.subscribers("ssub") == 1 && node.subCmds.Load() == 2 })
		})
	}
}

// The ack of the PubSub's own SUnsubscribe is not a trigger.
func TestPubSubSunsubscribeAckIsNotATrigger887(t *testing.T) {
	cl := newRPCluster(t, 1)
	ps := routedPubSub(t, cl, false)
	node := cl.nodes[0]
	if err := ps.SSubscribe(rpCtx(t), "foo"); err != nil {
		t.Fatal(err)
	}
	poll(t, "the SSUBSCRIBE", 5*time.Second, func() bool { return node.subscribers("ssub") == 1 })
	accepted := node.acceptedConns()
	if err := ps.SUnsubscribe(rpCtx(t), "foo"); err != nil {
		t.Fatal(err)
	}
	poll(t, "the SUNSUBSCRIBE", 5*time.Second, func() bool { return node.subscribers("ssub") == 0 })
	time.Sleep(400 * time.Millisecond)
	if n := node.subCmds.Load(); n != 1 {
		t.Errorf("%d SSUBSCRIBEs, want 1: the ack of the caller's own SUnsubscribe made the PubSub subscribe again", n)
	}
	if n := node.acceptedConns(); n != accepted {
		t.Errorf("the PubSub dialled %d more conns after its own SUnsubscribe", n-accepted)
	}
}

// -MOVED and -CLUSTERDOWN answers to an SSUBSCRIBE are triggers (the
// subscription did not take); a -CROSSSLOT is the caller's mistake and is not
// retried.
func TestPubSubControlErrorTriggers887(t *testing.T) {
	for _, tc := range []struct {
		errReply string
		retry    bool
	}{
		{"MOVED 12182 127.0.0.1:1", true},
		{"CLUSTERDOWN The cluster is down", true},
		{"CROSSSLOT Keys in request don't hash to the same slot", false},
		{"ERR unknown command 'ssubscribe'", false},
	} {
		t.Run(tc.errReply, func(t *testing.T) {
			cl := newRPCluster(t, 1)
			node := cl.nodes[0]
			node.setSSubErrs(tc.errReply)
			ps := routedPubSub(t, cl, false)
			if err := ps.SSubscribe(rpCtx(t), "foo"); err != nil {
				t.Fatal(err)
			}
			if tc.retry {
				poll(t, "the second SSUBSCRIBE after the error", 5*time.Second, func() bool { return node.subscribers("ssub") == 1 })
				return
			}
			time.Sleep(600 * time.Millisecond)
			if n := node.subCmds.Load(); n != 1 {
				t.Errorf("%d SSUBSCRIBEs after %q, want 1: a caller's error must not loop", n, tc.errReply)
			}
		})
	}
}

// A trigger from the new conn that arrives while the reroute loop is still
// running (the handover) is not lost: the conn is up, so no close hook says
// it, and the loop's tail starts one more reroute. Here the slot moves a
// second time right after the replay on the first new owner; the hook holds the
// loop until the unsolicited sunsubscribe has reached the PubSub.
func TestClusterRerouteTriggerDuringTheHandoverIsKept887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const ch = "foo"
	slot := int(Slot(ch))
	a := cl.owner(slot)
	b, c := cl.nodes[0], cl.nodes[1]
	if a == b || a == c {
		t.Fatal("test setup: the owner is one of the targets")
	}
	ps, err := cc.SSubscribe(rpCtx(t), ch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return a.subscribers("ssub") == 1 })

	var held atomic.Int64
	hook := func(p *PubSub) {
		if p != ps {
			return
		}
		if held.Add(1) > 1 {
			return
		}
		// Hold the first loop until the trigger of the new conn was recorded.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			p.reconnectingMu.Lock()
			pending := p.pendingReroute
			p.reconnectingMu.Unlock()
			if pending {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	testHookReplayed.Store(&hook)
	t.Cleanup(func() { testHookReplayed.Store(nil) })
	// The slot moves a second time right after b acks the replay.
	b.setOnSSub(func(n *rpNode, _ *rpConn, ch string) {
		b.setOnSSub(nil)
		cl.moveSlot(slot, c, 0)
	})
	cl.moveSlot(slot, b, 0)
	poll(t, "SSUBSCRIBE on the last owner", 15*time.Second, func() bool { return c.subscribers("ssub") == 1 })
	if held.Load() == 0 {
		t.Error("the loop's hook never ran")
	}
}

// The old conn is cut off before the new one is bound: with the loop held after
// the replay (the old conn still open, not yet released), a message the cluster
// delivers on both reaches the caller once. This is the hold the public test of
// the same name cannot make: there the loop has released the old conn before
// the probe comes back.
func TestClusterRerouteOldConnIsCutOffWhileTheLoopIsHeld887(t *testing.T) {
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
			cl.publish("x", "probe")
		}
	})
	hook := func(p *PubSub) {
		if p != ps {
			return
		}
		// The old conn is still open: wait for the probe, then give a
		// duplicate time to arrive through the old conn.
		deadline := time.Now().Add(3 * time.Second)
		for rc.count("probe") == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(200 * time.Millisecond)
	}
	testHookReplayed.Store(&hook)
	t.Cleanup(func() { testHookReplayed.Store(nil) })
	cl.moveSlot(slot, to, 0)
	poll(t, "SSUBSCRIBE on the new owner", 10*time.Second, func() bool { return to.subscribers("ssub") == 1 })
	time.Sleep(400 * time.Millisecond)
	if n := rc.count("probe"); n != 1 {
		t.Errorf("the probe was delivered %d times, want 1", n)
	}
}

// Close wins every tie with the backoff timer: a loop that finds both ready
// must not go on to dial a conn nobody owns.
func TestPubSubSleepIsClosedFirst887(t *testing.T) {
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
	_ = ps.Close()
	for i := 0; i < 300; i++ {
		if ps.sleep(0) {
			t.Fatalf("iteration %d: sleep returned true on a closed PubSub (a select with both ready picked the timer)", i)
		}
	}
}

// Close lands after the reroute loop dialled and before it installs the conn:
// the loop sees the PubSub closed under the lock, installs nothing and releases
// the conn it dialled (a conn installed on a closed PubSub would be nobody's to
// release).
func TestClusterRerouteDoesNotInstallAConnOnAClosedPubSub887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	ps, err := cc.Subscribe(rpCtx(t), "news")
	if err != nil {
		t.Fatal(err)
	}
	poll(t, "the subscription", 5*time.Second, func() bool { return cl.holder("sub") != nil })
	first := cl.holder("sub")

	reached := make(chan struct{})
	proceed := make(chan struct{})
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

	first.kill()
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
		t.Errorf("the loop installed a conn on a closed PubSub")
	}
}

// A report from the old conn that was in flight when the loop installed the new
// one (the router's callback read its target before the install cleared the
// router, then waited for mu) must not start a second reroute: the replay
// already answers it. The loop is held after the install and the old conn's
// router reports in that window.
func TestClusterRerouteIgnoresAReportFromTheReplacedConn887(t *testing.T) {
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
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return from.subscribers("ssub") == 1 })

	ps.mu.Lock()
	oldRouter := ps.conn.state.router
	ps.mu.Unlock()
	var held atomic.Int64
	hook := func(p *PubSub) {
		if p != ps || held.Add(1) > 1 {
			return
		}
		// After the install: the replaced conn's late reports.
		p.onShardUnsubscribed(oldRouter, ch)
		p.onControlError(oldRouter, "MOVED "+strconv.Itoa(slot)+" 127.0.0.1:1")
		p.reconnectingMu.Lock()
		pending := p.pendingReroute
		p.reconnectingMu.Unlock()
		if pending {
			t.Error("a report from the replaced conn set pendingReroute")
		}
	}
	testHookReplayed.Store(&hook)
	t.Cleanup(func() { testHookReplayed.Store(nil) })
	cl.moveSlot(slot, to, 0)
	poll(t, "SSUBSCRIBE on the new owner", 10*time.Second, func() bool { return to.subscribers("ssub") == 1 })
	time.Sleep(700 * time.Millisecond)
	if n := to.subCmds.Load(); n != 1 {
		t.Errorf("the new owner saw %d SSUBSCRIBE commands, want 1: a late report of the old conn made another reroute", n)
	}
}
