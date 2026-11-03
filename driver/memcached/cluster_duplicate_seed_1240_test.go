package memcached

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// poolLoops1240 counts the pool health goroutines of the process. Every node
// Client runs one from NewClient until Close, whether or not it ever dialled.
func poolLoops1240() int {
	buf := make([]byte, 1<<22)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "healthLoop") {
			n++
		}
	}
	return n
}

// waitLoops1240 polls until cond holds for the pool goroutine count, failing the
// test after a bound.
func waitLoops1240(t *testing.T, what string, cond func(int) bool) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := poolLoops1240()
		if cond(n) {
			return n
		}
		if time.Now().After(deadline) {
			t.Errorf("%s: %d pool goroutines", what, n)
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The memcached cluster keeps its nodes in a slice, one per Addrs entry, so a
// repeated address is two Clients that both stay reachable and both close:
// unlike the redis cluster's map, nothing is overwritten. The test pins that a
// repeated address leaves no Client open after Close.
func TestClusterDuplicateAddrLeavesNoClientBehind1240(t *testing.T) {
	_, addrs := startFakeCluster(t, 2)
	base := poolLoops1240()
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:               []string{addrs[0], addrs[1], addrs[0], addrs[0]},
		DialTimeout:         2 * time.Second,
		HealthProbeInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	want := base + len(cc.nodes)
	if got := waitLoops1240(t, "node Clients to start", func(n int) bool { return n >= want }); got > want {
		t.Errorf("%d pool goroutines for %d node Clients (base %d)", got-base, len(cc.nodes), base)
	}
	ctx := context.Background()
	for _, n := range cc.nodes {
		if err := n.client.Ping(ctx); err != nil {
			t.Fatalf("Ping %s: %v", n.addr, err)
		}
	}
	if err := cc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitLoops1240(t, "after Close", func(n int) bool { return n <= base })
}
