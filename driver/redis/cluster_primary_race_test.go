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
// the command's connection failed. This test makes both happen many times, from
// several goroutines released together by one barrier, with nothing in the test
// ordering one access before the other: under -race the detector reports any
// pair of those accesses that has no happens-before edge, whether or not they
// overlap in time, so the report does not depend on a lucky interleaving.
//
//   - Retry goroutines run commands whose call fails with a network-style error
//     right away: the retry path reads node.primary, then reads the topology
//     itself (which writes it).
//   - Redirect goroutines run commands whose first attempt is answered with
//     -MOVED: the redirect reads the topology (which writes node.primary).
//
// No sleeps; the only synchronisation is the start barrier and the final wait.
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		retryers  = 4
		redirects = 2
		rounds    = 60
	)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	fail := func(format string, a ...any) {
		mu.Lock()
		failures = append(failures, fmt.Sprintf(format, a...))
		mu.Unlock()
	}
	for g := 0; g < retryers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				err := cc.doWithRetry(ctx, key, func(context.Context, *Client) error {
					return errors.New("simulated connection failure")
				})
				if err == nil || !strings.Contains(err.Error(), "simulated connection failure") {
					fail("a command whose connection failed returned %v", err)
					return
				}
			}
		}()
	}
	for g := 0; g < redirects; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < rounds; i++ {
				moved := true
				err := cc.doWithRetry(ctx, key, func(context.Context, *Client) error {
					if moved {
						moved = false
						return &RedisError{Prefix: "MOVED", Msg: fmt.Sprintf("%d %s", slot, owner.addr)}
					}
					return nil
				})
				if err != nil {
					fail("a redirected command returned %v", err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	for _, f := range failures {
		t.Error(f)
	}
}
