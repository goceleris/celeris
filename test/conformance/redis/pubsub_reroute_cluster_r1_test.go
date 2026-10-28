//go:build redis_cluster

package redis_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
)

type r1Counter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *r1Counter) get(k string) int { c.mu.Lock(); defer c.mu.Unlock(); return c.n[k] }

func r1Collect(ps *redis.PubSub) *r1Counter {
	c := &r1Counter{n: map[string]int{}}
	go func() {
		for m := range ps.Channel() {
			c.mu.Lock()
			c.n[m.Channel+"/"+m.Payload]++
			c.mu.Unlock()
		}
	}()
	return c
}

// Two slots on one PubSub (documented undefined behaviour): the channel of the
// first slot keeps delivering, as it did before the PubSub followed its slot.
// The second channel is on another node (the SSUBSCRIBE is answered -MOVED) and
// is not served. A reroute that replayed both channels in one SSUBSCRIBE would
// get -CROSSSLOT from Redis and leave both dead.
func TestClusterSecondSlotOnOnePubSubKeepsTheFirstDelivering(t *testing.T) {
	cc := newClusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const a, b = "{l22-a}.x", "{l22-b}.y"
	if redis.Slot(a) == redis.Slot(b) {
		t.Fatal("test setup: same slot")
	}
	ps, err := cc.SSubscribe(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ps.Close() }()
	rc := r1Collect(ps)
	time.Sleep(300 * time.Millisecond)
	_ = ps.SSubscribe(ctx, b) // not served when it is on another node; the call itself returns nil
	time.Sleep(2 * time.Second)
	for i := 0; i < 3; i++ {
		_, _ = cc.SPublish(ctx, a, fmt.Sprintf("m%d", i))
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if n := rc.get(fmt.Sprintf("%s/m%d", a, i)); n != 1 {
			t.Errorf("%s m%d delivered %d times, want 1: the first channel died with the second", a, i, n)
		}
	}
}

// ssubConnIDs returns the ids of the connections of the node that hold a shard
// subscription (CLIENT LIST shows ssub=N).
func ssubConnIDs(ctx context.Context, t *testing.T, c *redis.Client) []string {
	t.Helper()
	v, err := c.Do(ctx, "CLIENT", "LIST")
	if err != nil {
		t.Fatalf("CLIENT LIST: %v", err)
	}
	var ids []string
	for _, line := range strings.Split(string(v.Str), "\n") {
		if !strings.Contains(line, " ssub=") || strings.Contains(line, " ssub=0 ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if strings.HasPrefix(f, "id=") {
				ids = append(ids, f)
			}
		}
	}
	return ids
}

// Redis answers SUNSUBSCRIBE of a channel the connection holds no subscription
// for, in a slot the node does not serve, with -MOVED. That answers the
// caller's harmless call; it must not rebuild the healthy subscription: the
// connection that holds it stays the same one.
func TestClusterForeignSUnsubscribeKeepsTheSubscriptionConn(t *testing.T) {
	addrs := clusterAddrsFromEnv(t)
	cc := newClusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const a, foreign = "{l22-a}.x", "{l22-b}.never"
	seed, err := redis.NewClient(addrs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seed.Close() }()
	ownerAddr, _, _, _ := clusterOwnerOf(ctx, t, seed, int(redis.Slot(a)))
	owner, err := redis.NewClient(ownerAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if ownerAddr2, _, _, _ := clusterOwnerOf(ctx, t, seed, int(redis.Slot(foreign))); ownerAddr2 == ownerAddr {
		t.Fatal("test setup: the foreign slot is served by the same node")
	}

	ps, err := cc.SSubscribe(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ps.Close() }()
	time.Sleep(500 * time.Millisecond)
	before := ssubConnIDs(ctx, t, owner)
	if len(before) != 1 {
		t.Fatalf("the owner has %d connections holding a shard subscription before the call, want 1", len(before))
	}
	if err := ps.SUnsubscribe(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	after := ssubConnIDs(ctx, t, owner)
	if len(after) != 1 || after[0] != before[0] {
		t.Errorf("the subscription's connection was %v, now %v: a SUnsubscribe of a channel the PubSub never had rebuilt it", before, after)
	}
}
