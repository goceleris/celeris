//go:build redis_cluster

package redis_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
)

// A channel of a second slot that the same node owns is accepted by the node.
// When that slot moves away the node sends sunsubscribe for it; the PubSub
// serves the slot of its first channel, so the move must not rebuild the
// subscription it serves: the connection that holds it stays the same one.
func TestClusterMoveOfASecondSlotOnTheSameNodeKeepsTheSubscriptionConn(t *testing.T) {
	addrs := clusterAddrsFromEnv(t)
	cc := newClusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	seed, err := redis.NewClient(addrs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seed.Close() }()

	const a = "{l22-a}.x"
	ownerAddr, ownerID, otherAddr, otherID := clusterOwnerOf(ctx, t, seed, int(redis.Slot(a)))
	// A channel of another slot that the same node serves.
	var second string
	for i := 0; i < 200 && second == ""; i++ {
		cand := fmt.Sprintf("{l22-n2-%d}.y", i)
		if redis.Slot(cand) == redis.Slot(a) {
			continue
		}
		if addr, _, _, _ := clusterOwnerOf(ctx, t, seed, int(redis.Slot(cand))); addr == ownerAddr {
			second = cand
		}
	}
	if second == "" {
		t.Fatal("test setup: no second slot on the owner of the first")
	}
	owner, err := redis.NewClient(ownerAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	other, err := redis.NewClient(otherAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()

	ps, err := cc.SSubscribe(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ps.Close() }()
	if err := ps.SSubscribe(ctx, second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	before := ssubConnIDs(ctx, t, owner)
	if len(before) != 1 {
		t.Fatalf("the owner has %d connections holding a shard subscription, want 1", len(before))
	}

	slot2 := int(redis.Slot(second))
	moveEmptySlot(ctx, t, owner, other, ownerID, otherID, slot2)
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		moveEmptySlot(cctx, t, other, owner, otherID, ownerID, slot2)
	}()
	time.Sleep(1500 * time.Millisecond)
	after := ssubConnIDs(ctx, t, owner)
	if len(after) != 1 || after[0] != before[0] {
		t.Errorf("the subscription's connection was %v, now %v: the move of a slot the PubSub does not serve rebuilt it", before, after)
	}
}
