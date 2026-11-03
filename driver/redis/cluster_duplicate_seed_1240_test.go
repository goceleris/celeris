package redis

import (
	"context"
	"net"
	"testing"
	"time"
)

// healthLoops counts the pool health goroutines of the process. Every Client
// runs two (its command pool and its pub/sub pool) from NewClient until Close,
// whether or not it ever dialled, so a Client nobody closed shows up here even
// though it holds no connection.
func healthLoops() int { return goroutinesIn("healthLoop") }

// clientsOf1240 is how many Clients the cluster stores: its nodes.
func clientsOf1240(cc *ClusterClient) int {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	return len(cc.nodes)
}

// openClients1240 waits for the health goroutines to reach base plus two per
// stored Client and then asserts there are no more: a Client built and dropped
// without Close has its two left over.
func openClients1240(t *testing.T, cc *ClusterClient, base int) {
	t.Helper()
	want := base + 2*clientsOf1240(cc)
	poll(t, "the node Clients' pool goroutines to start", 5*time.Second, func() bool { return healthLoops() >= want })
	if got := healthLoops(); got > want {
		t.Errorf("%d pool goroutines with %d stored node Clients (base %d): %d Clients are open that the cluster does not hold",
			got-base, clientsOf1240(cc), base, (got-want)/2)
	}
}

// closedClients1240 asserts that after Close no pool goroutine is left beyond
// base, and no connection is left open on the fake.
func closedClients1240(t *testing.T, cl *rpCluster, base int) {
	t.Helper()
	var got int
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got = healthLoops(); got <= base {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("after Close %d pool goroutines are left (%d Clients never closed)", got-base, (got-base)/2)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	poll(t, "the fake's connections to close", 5*time.Second, func() bool { return cl.liveConns() == 0 })
}

// useEveryNode1240 sends a PING through every stored node Client, so each holds
// a connection on the fake.
func useEveryNode1240(t *testing.T, cc *ClusterClient) {
	t.Helper()
	cc.mu.RLock()
	nodes := make([]*clusterNode, 0, len(cc.nodes))
	for _, n := range cc.nodes {
		nodes = append(nodes, n)
	}
	cc.mu.RUnlock()
	for _, n := range nodes {
		if err := n.client.Ping(rpCtx(t)); err != nil {
			t.Fatalf("PING %s: %v", n.addr, err)
		}
	}
}

// A seed address listed twice built a Client for each entry, and initNodes kept
// the second in the map under the same key: the first, with its pools, was never
// closed.
func TestClusterDuplicateSeedLeavesNoClientBehind1240(t *testing.T) {
	cl := newRPCluster(t, 3)
	base := healthLoops()
	a := cl.nodes[0].addr
	cc, err := NewClusterClient(ClusterConfig{Addrs: []string{a, a, a}, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	if got := clientsOf1240(cc); got != 3 {
		t.Fatalf("stored %d node Clients, want the 3 nodes of the topology", got)
	}
	openClients1240(t, cc, base)
	useEveryNode1240(t, cc)
	if err := cc.Close(); err != nil {
		t.Fatal(err)
	}
	closedClients1240(t, cl, base)
}

// The duplicate need not be adjacent, and the other seeds are kept.
func TestClusterDuplicateSeedAmongOthersKeepsEachOnce1240(t *testing.T) {
	cl := newRPCluster(t, 3)
	base := healthLoops()
	addrs := cl.addrs()
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{addrs[0], addrs[1], addrs[0], addrs[2], addrs[1]},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	for _, a := range addrs {
		cc.mu.RLock()
		_, ok := cc.nodes[a]
		cc.mu.RUnlock()
		if !ok {
			t.Errorf("seed %s is not a stored node", a)
		}
	}
	openClients1240(t, cc, base)
	useEveryNode1240(t, cc)
	if err := cc.Close(); err != nil {
		t.Fatal(err)
	}
	closedClients1240(t, cl, base)
}

// Two seeds that are the same node under two spellings are two map keys, so
// nothing is overwritten at init. The one the topology does not name is closed
// by the first topology read, and later reads add nothing: it stays at one
// Client per node.
func TestClusterSeedsReachingTheSameNodeLeaveNoClientBehind1240(t *testing.T) {
	cl := newRPCluster(t, 3)
	base := healthLoops()
	a := cl.nodes[0].addr
	_, port, err := net.SplitHostPort(a)
	if err != nil {
		t.Fatal(err)
	}
	mapped := net.JoinHostPort("::ffff:127.0.0.1", port) // the same node, spelled differently
	cc, err := NewClusterClient(ClusterConfig{Addrs: []string{a, mapped}, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	if got := clientsOf1240(cc); got != 3 {
		t.Fatalf("stored %d node Clients, want the 3 nodes of the topology", got)
	}
	for i := range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := cc.refreshTopology(ctx)
		cancel()
		if err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	openClients1240(t, cc, base)
	useEveryNode1240(t, cc)
	if err := cc.Close(); err != nil {
		t.Fatal(err)
	}
	closedClients1240(t, cl, base)
}

// The topology refresh reuses the node it already holds for an address and adds
// only the ones it lacks; it never replaces a stored Client.
func TestClusterRefreshNeverReplacesAStoredNodeClient1240(t *testing.T) {
	cl := newRPCluster(t, 3)
	base := healthLoops()
	cc := newRPClusterClient(t, cl)
	before := map[string]*Client{}
	cc.mu.RLock()
	for a, n := range cc.nodes {
		before[a] = n.client
	}
	cc.mu.RUnlock()
	for i := range 5 {
		if err := cc.refreshTopology(rpCtx(t)); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	cc.mu.RLock()
	for a, c := range before {
		if n, ok := cc.nodes[a]; !ok || n.client != c {
			t.Errorf("node %s: the stored Client changed across refreshes", a)
		}
	}
	cc.mu.RUnlock()
	if got := clientsOf1240(cc); got != len(before) {
		t.Errorf("%d node Clients after refreshes, had %d", got, len(before))
	}
	openClients1240(t, cc, base)
	if err := cc.Close(); err != nil {
		t.Fatal(err)
	}
	closedClients1240(t, cl, base)
}
