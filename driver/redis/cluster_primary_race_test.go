package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// A topology read (applyTopology) sets clusterNode.primary of the nodes it finds,
// including nodes that already exist. The retry path of a command reads it when
// the command's connection failed. This test runs the two at once under
// -race: no sleeps; channels order the steps so the race detector sees both
// accesses with nothing between them that orders one before the other.
//
//   - Command R takes its node from the slot table, then its call fails with a
//     network-style error: the retry path reads node.primary.
//   - Command W's first attempt is answered with -MOVED once R is inside its call
//     (R closed routed), so W reads the topology, which writes node.primary.
func TestClusterTopologyReadDoesNotRaceWithTheRetryPath(t *testing.T) {
	node1, node2, node3 := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr(), node2.Addr(), node3.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	const key = "hello"
	slot := Slot(key)
	owner := cc.route(slot)
	if owner == nil {
		t.Fatal("no node owns the slot of " + key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := 0; i < 5; i++ {
		routed := make(chan struct{})
		var wg sync.WaitGroup
		var rErr, wErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			rErr = cc.doWithRetry(ctx, key, func(context.Context, *Client) error {
				close(routed)
				return errors.New("simulated connection failure")
			})
		}()
		go func() {
			defer wg.Done()
			first := true
			wErr = cc.doWithRetry(ctx, key, func(context.Context, *Client) error {
				if first {
					first = false
					<-routed
					return &RedisError{Prefix: "MOVED", Msg: fmt.Sprintf("%d %s", slot, owner.addr)}
				}
				return nil
			})
		}()
		wg.Wait()
		if wErr != nil {
			t.Fatalf("round %d: the command that was redirected: %v", i, wErr)
		}
		if rErr == nil || !strings.Contains(rErr.Error(), "simulated connection failure") {
			t.Fatalf("round %d: the command whose connection failed returned %v", i, rErr)
		}
	}
}
