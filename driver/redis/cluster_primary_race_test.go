package redis

import (
	"context"
	"errors"
	"fmt"
	"runtime"
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

// The test above relies on many repetitions. The two below run the two accesses
// once and put the write first by construction, so under -race the detector
// reports the pair on every run if clusterNode.primary is read without c.mu.
// Each covers one of the two reads of primary in doWithRetryCmd: the check for a
// replica (the first read after a failed call) and the check for a lost primary
// (the second).
//
// The detector reports two accesses with no happens-before edge between them, so
// two things have to hold: the write must run before the read (right after its
// read the retry path writes the node's failing flag, which shares a word with
// primary, and the detector may no longer hold the read by the time the write
// comes), and nothing may carry the write to the read.
//
//   - The retry goroutine routes the command (it takes c.mu for reading) and calls
//     fn, which returns an error that is not a server reply. The test waits until
//     the goroutine is in fn (first test) or in beforeLostPrimaryCheck (second
//     test), so nothing the goroutine does after that takes c.mu before the read.
//   - applyTopology is called directly with a CLUSTER SLOTS reply fetched before the
//     retry goroutine starts, so it does no I/O. It takes c.mu and writes primary of
//     every node of the reply.
//   - The goroutine reads primary only after applyTopology has returned. It learns
//     that from len(written), not from a receive: the Go memory model lets any
//     number of goroutines call len on a channel without synchronisation, and
//     it orders nothing. (A receive or an atomic load would carry the write to
//     the read.)
//
// With the read under c.mu (read lock) the read takes the lock applyTopology
// released, which orders the write before it, and nothing is reported.
//
// The first test needs no seam. The second needs a point between the two reads:
// the first read, when it takes c.mu, orders every later read of the same
// goroutine after applyTopology, so without a point between them the second read
// cannot be reached unordered. beforeLostPrimaryCheck is that point.

// TestClusterRetryPathReplicaCheckFollowsTopologyWrite: the read in the replica
// check.
func TestClusterRetryPathReplicaCheckFollowsTopologyWrite(t *testing.T) {
	readsPrimaryAfterTopologyWrite(t, false)
}

// TestClusterRetryPathLostPrimaryCheckFollowsTopologyWrite: the read in the
// lost-primary check.
func TestClusterRetryPathLostPrimaryCheckFollowsTopologyWrite(t *testing.T) {
	readsPrimaryAfterTopologyWrite(t, true)
}

// readsPrimaryAfterTopologyWrite runs one command whose call fails with a
// network-style error and, while it waits (in fn, or in beforeLostPrimaryCheck
// when atLostPrimaryCheck is set), applies a topology.
func readsPrimaryAfterTopologyWrite(t *testing.T, atLostPrimaryCheck bool) {
	node1, node2, node3 := setupCluster(t)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       []string{node1.Addr(), node2.Addr(), node3.Addr()},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reply, err := cc.anyNode().client.Do(ctx, "CLUSTER", "SLOTS")
	if err != nil {
		t.Fatalf("CLUSTER SLOTS: %v", err)
	}

	waiting := make(chan struct{})
	written := make(chan struct{}, 1)
	waitForWrite := func() {
		close(waiting)
		for len(written) == 0 {
			runtime.Gosched()
		}
	}
	if atLostPrimaryCheck {
		cc.beforeLostPrimaryCheck = waitForWrite // set before the goroutine starts
	}
	done := make(chan error, 1)
	go func() {
		done <- cc.doWithRetry(ctx, "hello", func(context.Context, *Client) error {
			if !atLostPrimaryCheck {
				waitForWrite()
			}
			return errors.New("simulated connection failure")
		})
	}()
	<-waiting

	// A sequence number no read of the client has reached, so that the table is
	// applied and primary is written.
	err = cc.applyTopology(ctx, reply, 1<<40)
	written <- struct{}{} // whatever applyTopology returned: lets the goroutine go on
	if err != nil {
		t.Errorf("applyTopology: %v", err)
	}
	err = <-done
	if err == nil || !strings.Contains(err.Error(), "simulated connection failure") {
		t.Errorf("a command whose connection failed returned %v", err)
	}
}
