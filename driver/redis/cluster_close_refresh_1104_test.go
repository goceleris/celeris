package redis

import (
	"context"
	"errors"
	"testing"
	"time"
)

// closedClusterWithSlots1104 returns a cluster client that has been closed and
// a CLUSTER SLOTS reply it fetched before the Close: the state of a refresh whose
// Do finished just before Close and has not yet taken c.mu.
func closedClusterWithSlots1104(t *testing.T) (*ClusterClient, *Value, *fakeClusterNode) {
	t.Helper()
	node1, _, _ := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{Addrs: []string{node1.Addr()}, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	v, err := cc.anyNode().client.Do(context.Background(), "CLUSTER", "SLOTS")
	if err != nil {
		t.Fatalf("CLUSTER SLOTS: %v", err)
	}
	if err := cc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return cc, v, node1
}

// connsOf1104 is how many connections the fake node has accepted so far.
func connsOf1104(n *fakeClusterNode) int {
	n.fake.mu.Lock()
	defer n.fake.mu.Unlock()
	return len(n.fake.conns)
}

// panicOf1104 runs f and returns the panic it ended with, or nil.
func panicOf1104(f func()) (panicked any) {
	defer func() { panicked = recover() }()
	f()
	return nil
}

// A refresh that takes c.mu after Close wrote into the nil c.nodes that
// closeNodes had left and ended the process with "assignment to entry in nil
// map" (after dialling a node nothing would close). It must return ErrClosed
// and dial nothing.
func TestClusterRefreshAfterCloseDoesNotWriteToTheNodes1104(t *testing.T) {
	const iters = 30
	panics, dialled, wrongErr := 0, 0, 0
	for range iters {
		cc, v, node1 := closedClusterWithSlots1104(t)
		before := connsOf1104(node1)
		var err error
		if p := panicOf1104(func() { err = cc.applyTopology(context.Background(), v) }); p != nil {
			panics++
			continue
		}
		if connsOf1104(node1) != before {
			dialled++
		}
		if !errors.Is(err, ErrClosed) {
			wrongErr++
		}
	}
	if panics != 0 || dialled != 0 || wrongErr != 0 {
		t.Errorf("a topology refresh after Close: %d of %d panicked, %d dialled a node, %d did not return ErrClosed", panics, iters, dialled, wrongErr)
	}
}

// getOrCreateNode is the other writer of c.nodes (a MOVED or ASK redirect to an
// address not known yet): after Close it dialled the node and stored it in the
// nil map, which panics. It must return ErrClosed and dial nothing.
func TestClusterGetOrCreateNodeAfterCloseDoesNotWriteToTheNodes1104(t *testing.T) {
	const iters = 30
	panics, dialled, wrongErr := 0, 0, 0
	for range iters {
		cc, _, node1 := closedClusterWithSlots1104(t)
		before := connsOf1104(node1)
		var err error
		if p := panicOf1104(func() { _, err = cc.getOrCreateNode(node1.Addr()) }); p != nil {
			panics++
			continue
		}
		if connsOf1104(node1) != before {
			dialled++
		}
		if !errors.Is(err, ErrClosed) {
			wrongErr++
		}
	}
	if panics != 0 || dialled != 0 || wrongErr != 0 {
		t.Errorf("getOrCreateNode after Close: %d of %d panicked, %d dialled a node, %d did not return ErrClosed", panics, iters, dialled, wrongErr)
	}
}
