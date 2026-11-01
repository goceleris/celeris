package redis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
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

// panicOf1104 runs f and returns the panic it ended with, or nil.
func panicOf1104(f func()) (panicked any) {
	defer func() { panicked = recover() }()
	f()
	return nil
}

// clientClosed1104 reports whether err is what a command on a closed Client
// answers.
func clientClosed1104(err error) bool {
	return errors.Is(err, ErrClosed) || errors.Is(err, async.ErrPoolClosed)
}

// nodesNil1104 reports whether closeNodes' nil c.nodes is still nil, i.e. nothing
// was stored after Close.
func nodesNil1104(c *ClusterClient) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.nodes == nil
}

// A refresh that takes c.mu after Close wrote into the nil c.nodes that
// closeNodes had left and ended the process with "assignment to entry in nil
// map". It must return ErrClosed and store nothing.
func TestClusterRefreshAfterCloseDoesNotWriteToTheNodes1104(t *testing.T) {
	const iters = 30
	panics, wrongErr, stored := 0, 0, 0
	for range iters {
		cc, v, _ := closedClusterWithSlots1104(t)
		var err error
		if p := panicOf1104(func() { err = cc.applyTopology(context.Background(), v, 0) }); p != nil {
			panics++
			continue
		}
		if !errors.Is(err, ErrClosed) {
			wrongErr++
		}
		if !nodesNil1104(cc) {
			stored++
		}
	}
	if panics != 0 || wrongErr != 0 || stored != 0 {
		t.Errorf("a topology refresh after Close: %d of %d panicked, %d did not return ErrClosed, %d stored a node", panics, iters, wrongErr, stored)
	}
}

// getOrCreateNode is the other writer of c.nodes (a MOVED or ASK redirect to an
// address not known yet): after Close it must return ErrClosed and store
// nothing. It gets there through its early closed check, which is not reachable
// on its own: without it the node is dialled (a Client dials lazily, so nothing
// observable happens) and installNode answers ErrClosed too, with the same
// result; the check only saves building a client for a closed cluster. The
// check under c.mu is tested through installNode below.
func TestClusterGetOrCreateNodeAfterCloseDoesNotWriteToTheNodes1104(t *testing.T) {
	const iters = 30
	panics, wrongErr, stored := 0, 0, 0
	for range iters {
		cc, _, node1 := closedClusterWithSlots1104(t)
		var err error
		if p := panicOf1104(func() { _, err = cc.getOrCreateNode(node1.Addr()) }); p != nil {
			panics++
			continue
		}
		if !errors.Is(err, ErrClosed) {
			wrongErr++
		}
		if !nodesNil1104(cc) {
			stored++
		}
	}
	if panics != 0 || wrongErr != 0 || stored != 0 {
		t.Errorf("getOrCreateNode after Close: %d of %d panicked, %d did not return ErrClosed, %d stored a node", panics, iters, wrongErr, stored)
	}
}

// installNode is the part of getOrCreateNode under c.mu, which a Close that ran
// while the node dialled reaches with no early check in the way: it must return
// ErrClosed, store nothing and close the client it was given. Its other two
// outcomes are pinned too, so the test is of the function and not only of this
// case.
func TestClusterInstallNode1104(t *testing.T) {
	const iters = 30
	panics, wrongErr, stored, open := 0, 0, 0, 0
	for range iters {
		cc, _, node1 := closedClusterWithSlots1104(t)
		client, err := cc.dialNode(node1.Addr())
		if err != nil {
			t.Fatalf("dialNode: %v", err)
		}
		n := &clusterNode{addr: node1.Addr(), client: client, primary: true}
		var got *clusterNode
		if p := panicOf1104(func() { got, err = cc.installNode(n) }); p != nil {
			panics++
			_ = client.Close()
			continue
		}
		if !errors.Is(err, ErrClosed) || got != nil {
			wrongErr++
		}
		if !nodesNil1104(cc) {
			stored++
		}
		if _, derr := client.Do(context.Background(), "PING"); !clientClosed1104(derr) {
			open++ // the client was not closed
			_ = client.Close()
		}
	}
	if panics != 0 || wrongErr != 0 || stored != 0 || open != 0 {
		t.Errorf("installNode after Close: %d of %d panicked, %d did not return (nil, ErrClosed), %d stored a node, %d left the client open", panics, iters, wrongErr, stored, open)
	}

	// On a live cluster a new address is stored and returned, and an address that
	// is there already returns the stored node and closes the new client.
	node1, _, _ := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{Addrs: []string{node1.Addr()}, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cc.Close() }()
	fresh, err := cc.dialNode("127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	n := &clusterNode{addr: "127.0.0.1:1", client: fresh, primary: true}
	if got, err := cc.installNode(n); err != nil || got != n {
		t.Fatalf("installNode(new address) = %v, %v; want the node itself", got, err)
	}
	dup, err := cc.dialNode("127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := cc.installNode(&clusterNode{addr: "127.0.0.1:1", client: dup}); err != nil || got != n {
		t.Fatalf("installNode(known address) = %v, %v; want the stored node", got, err)
	}
	if _, derr := dup.Do(context.Background(), "PING"); !clientClosed1104(derr) {
		t.Errorf("the duplicate client was not closed: %v", derr)
	}
}
