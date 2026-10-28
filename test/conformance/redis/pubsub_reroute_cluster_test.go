//go:build redis_cluster

package redis_test

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
)

// clusterOwnerOf returns the host:port and the node id of the primary that owns
// slot, and those of another primary.
func clusterOwnerOf(ctx context.Context, t *testing.T, c *redis.Client, slot int) (ownerAddr, ownerID, otherAddr, otherID string) {
	t.Helper()
	v, err := c.Do(ctx, "CLUSTER", "SLOTS")
	if err != nil {
		t.Fatalf("CLUSTER SLOTS: %v", err)
	}
	for _, r := range v.Array {
		if len(r.Array) < 3 || len(r.Array[2].Array) < 3 {
			continue
		}
		p := r.Array[2].Array
		addr := fmt.Sprintf("%s:%d", p[0].Str, p[1].Int)
		id := string(p[2].Str)
		if slot >= int(r.Array[0].Int) && slot <= int(r.Array[1].Int) {
			ownerAddr, ownerID = addr, id
		} else if otherAddr == "" {
			otherAddr, otherID = addr, id
		}
	}
	if ownerAddr == "" || otherAddr == "" {
		t.Fatalf("CLUSTER SLOTS names no owner for slot %d or no second primary", slot)
	}
	return
}

// moveEmptySlot moves slot from one primary to another, the way redis-cli does
// for a slot with no keys: IMPORTING on the target, MIGRATING on the source,
// then NODE on the target and on the source.
func moveEmptySlot(ctx context.Context, t *testing.T, from, to *redis.Client, fromID, toID string, slot int) {
	t.Helper()
	s := strconv.Itoa(slot)
	for _, step := range []struct {
		c    *redis.Client
		args []any
	}{
		{to, []any{"CLUSTER", "SETSLOT", s, "IMPORTING", fromID}},
		{from, []any{"CLUSTER", "SETSLOT", s, "MIGRATING", toID}},
		{to, []any{"CLUSTER", "SETSLOT", s, "NODE", toID}},
		{from, []any{"CLUSTER", "SETSLOT", s, "NODE", toID}},
	} {
		if _, err := step.c.Do(ctx, step.args...); err != nil {
			t.Fatalf("%v: %v", step.args, err)
		}
	}
}

// A shard subscription follows its slot on a real cluster: when the slot moves
// the server sends an unsolicited sunsubscribe and keeps the conn, and the
// PubSub subscribes again on the new owner and keeps delivering to the same
// channel (celeris#887 item 31). The slot goes back at the end.
func TestClusterSSubscribeFollowsASlotMove(t *testing.T) {
	addrs := clusterAddrsFromEnv(t)
	cc := newClusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const ch = "{l22-reroute}.ch"
	slot := int(redis.Slot(ch))
	seed, err := redis.NewClient(addrs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seed.Close() }()
	ownerAddr, ownerID, otherAddr, otherID := clusterOwnerOf(ctx, t, seed, slot)
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

	ps, err := cc.SSubscribe(ctx, ch)
	if err != nil {
		t.Fatalf("SSubscribe: %v", err)
	}
	defer func() { _ = ps.Close() }()
	var mu sync.Mutex
	got := map[string]int{}
	go func() {
		for m := range ps.Channel() {
			mu.Lock()
			got[m.Payload]++
			mu.Unlock()
		}
	}()
	count := func(p string) int { mu.Lock(); defer mu.Unlock(); return got[p] }

	// publishUntil publishes distinct payloads every 100 ms until one arrives:
	// what is published while the PubSub switches is lost, so one message
	// does not prove anything.
	publishUntil := func(prefix string) []string {
		var sent []string
		deadline := time.Now().Add(30 * time.Second)
		for i := 0; time.Now().Before(deadline); i++ {
			p := fmt.Sprintf("%s-%d", prefix, i)
			sent = append(sent, p)
			_, _ = cc.SPublish(ctx, ch, p)
			time.Sleep(100 * time.Millisecond)
			for _, q := range sent {
				if count(q) > 0 {
					return sent
				}
			}
		}
		t.Fatalf("no message of %q arrived within 30 s", prefix)
		return nil
	}
	publishUntil("before")

	moveEmptySlot(ctx, t, owner, other, ownerID, otherID, slot)
	// The slot goes back before the clients above are closed (defers run in
	// reverse order, a t.Cleanup would run after them).
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		moveEmptySlot(cctx, t, other, owner, otherID, ownerID, slot)
	}()
	sent := publishUntil("after-move")
	time.Sleep(300 * time.Millisecond)
	for _, p := range sent {
		if n := count(p); n > 1 {
			t.Errorf("payload %q delivered %d times, want at most once", p, n)
		}
	}
}
