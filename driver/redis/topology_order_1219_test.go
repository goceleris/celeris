package redis

// celeris#1219: an older CLUSTER SLOTS read that ended AFTER a newer one overwrote
// the slot table with its older answer (applyTopology had no ordering): a node
// that learned of a move late could put the table back to the state before the
// move, and the commands then took an extra -MOVED round or a stale route until the
// next refresh. Found by the review of #1215.

import (
	"context"
	"testing"
)

// Read 1 takes its table before a slot moves and is held; read 2 starts after the
// move, ends and is applied; read 1 is released and ends last. The table must keep
// read 2's answer. (Main: read 1 overwrites it, the slot routes to the old owner.)
func TestAnOlderTopologyReadEndingLastDoesNotOverwriteANewerOne1219(t *testing.T) {
	const key = "{order}k"
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	slot := int(Slot(key))
	from := cl.owner(slot)
	to := otherNode(cl, from)

	waitHeld, release := holdNextSlotsRead(t)
	r1 := make(chan error, 1)
	go func() { r1 <- cc.refreshTopology(rpCtx(t)) }() // registered first, takes the pre-move table
	waitHeld()
	cl.moveSlot(slot, to, 0)
	if err := cc.refreshTopology(rpCtx(t)); err != nil { // registered later, applied
		t.Fatal(err)
	}
	if r := cc.route(Slot(key)); r == nil || r.addr != to.addr {
		t.Fatal("the newer read did not put the slot on the new owner")
	}
	release()
	if err := <-r1; err != nil {
		t.Fatal(err)
	}
	if r := cc.route(Slot(key)); r == nil || r.addr != to.addr {
		t.Errorf("the table went back to the pre-move owner: the older read, which ended last, overwrote the newer one")
	}
}

// The order of registration decides, not the order of ending: reads that end in
// the order they started all apply (a refresh after a move updates the table), and
// so does a newer read after an older one that failed.
func TestTopologyReadsThatEndInOrderAllApply1219(t *testing.T) {
	const key = "{inorder}k"
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	slot := int(Slot(key))
	a := cl.owner(slot)
	b := otherNode(cl, a)
	ctx := rpCtx(t)
	for i, to := range []*rpNode{b, a, b, a} {
		cl.moveSlot(slot, to, 0)
		if err := cc.refreshTopology(ctx); err != nil {
			t.Fatal(err)
		}
		if r := cc.route(Slot(key)); r == nil || r.addr != to.addr {
			t.Fatalf("move %d: after the refresh the slot does not route to its new owner", i)
		}
	}
	// A read cut short by its caller's context applies nothing and does not
	// advance the order: the next read still applies.
	cl.moveSlot(slot, b, 0)
	cctx, cancel := context.WithCancel(ctx)
	waitHeld, release := holdNextSlotsRead(t)
	r := make(chan error, 1)
	go func() { r <- cc.refreshTopology(cctx) }()
	waitHeld()
	cancel()
	t.Logf("the cancelled read: %v", <-r)
	release()
	if err := cc.refreshTopology(ctx); err != nil {
		t.Fatal(err)
	}
	if rt := cc.route(Slot(key)); rt == nil || rt.addr != b.addr {
		t.Error("a read after a cancelled one did not apply")
	}
}
