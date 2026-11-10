//go:build redis_cluster

package redis_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
)

// celeris#1204: a PubSub that holds shard channels of two slots. The real
// cluster is the authority on what the server accepts: it takes SSUBSCRIBE of
// different slots as separate commands on one conn, and answers -CROSSSLOT to a
// command (SSUBSCRIBE or SUNSUBSCRIBE) whose channels span slots.

// r3Deliver publishes distinct payloads to ch every 100 ms until one arrives in
// rc (what is published while a PubSub switches is lost, so one message proves
// nothing).
func r3Deliver(ctx context.Context, t *testing.T, cc *redis.ClusterClient, rc *r1Counter, ch, prefix string) bool {
	t.Helper()
	var sent []string
	deadline := time.Now().Add(30 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		p := fmt.Sprintf("%s-%d", prefix, i)
		sent = append(sent, p)
		_, _ = cc.SPublish(ctx, ch, p)
		time.Sleep(100 * time.Millisecond)
		for _, q := range sent {
			if rc.get(ch+"/"+q) > 0 {
				return true
			}
		}
	}
	return false
}

// r3Delivered counts what rc received on ch for the payloads prefix-0..prefix-n.
func r3Delivered(rc *r1Counter, ch, prefix string, n int) int {
	got := 0
	for i := 0; i < n; i++ {
		got += rc.get(fmt.Sprintf("%s/%s-%d", ch, prefix, i))
	}
	return got
}

// r3ChannelOn returns a channel of a slot other than the one of avoid, owned by
// the node at wantAddr (same == true) or by another node (same == false).
func r3ChannelOn(ctx context.Context, t *testing.T, seed *redis.Client, avoid, wantAddr string, same bool) string {
	t.Helper()
	for i := 0; i < 500; i++ {
		cand := fmt.Sprintf("{l1204-%d}.y", i)
		if redis.Slot(cand) == redis.Slot(avoid) {
			continue
		}
		if addr, _, _, _ := clusterOwnerOf(ctx, t, seed, int(redis.Slot(cand))); (addr == wantAddr) == same {
			return cand
		}
	}
	t.Fatalf("test setup: no channel (same node: %v)", same)
	return ""
}

// Two slots of one node on one PubSub; the PubSub's conn dies and the reconnect
// replays both: the second channel is delivered again (the replay of #1154 left
// it out).
func TestClusterSecondSlotOfTheSameNodeSurvivesAReplay(t *testing.T) {
	addrs := clusterAddrsFromEnv(t)
	cc := newClusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	seed, err := redis.NewClient(addrs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seed.Close() }()
	const a = "{l1204-a}.x"
	ownerAddr, _, _, _ := clusterOwnerOf(ctx, t, seed, int(redis.Slot(a)))
	b := r3ChannelOn(ctx, t, seed, a, ownerAddr, true)
	owner, err := redis.NewClient(ownerAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()

	ps, err := cc.SSubscribe(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ps.Close() }()
	rc := r1Collect(ps)
	if err := ps.SSubscribe(ctx, b); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if !r3Deliver(ctx, t, cc, rc, a, "a0") || !r3Deliver(ctx, t, cc, rc, b, "b0") {
		t.Fatal("the channels of the two slots are not both delivered before the kill")
	}
	// The server hangs up on the PubSub's conn (the only pub/sub client of the
	// node here); the reconnect replays both channels.
	if _, err := owner.Do(ctx, "CLIENT", "KILL", "TYPE", "pubsub"); err != nil {
		t.Fatalf("CLIENT KILL: %v", err)
	}
	if !r3Deliver(ctx, t, cc, rc, a, "a1") {
		t.Error("the first channel is not delivered after the replay")
	}
	if !r3Deliver(ctx, t, cc, rc, b, "b1") {
		t.Error("the second channel (another slot of the same node) is not delivered after the replay")
	}
}

// The first channel's slot moves to the node that owns the second channel's
// slot: the PubSub follows it and now both are on its node, so both deliver.
func TestClusterServedSlotMovesToTheNodeOfTheSecondSlot(t *testing.T) {
	addrs := clusterAddrsFromEnv(t)
	cc := newClusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	seed, err := redis.NewClient(addrs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seed.Close() }()
	const a = "{l1204-m}.x"
	aAddr, aID, _, _ := clusterOwnerOf(ctx, t, seed, int(redis.Slot(a)))
	b := r3ChannelOn(ctx, t, seed, a, aAddr, false)
	bAddr, bID, _, _ := clusterOwnerOf(ctx, t, seed, int(redis.Slot(b)))
	from, err := redis.NewClient(aAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = from.Close() }()
	to, err := redis.NewClient(bAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = to.Close() }()

	ps, err := cc.SSubscribe(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ps.Close() }()
	rc := r1Collect(ps)
	if err := ps.SSubscribe(ctx, b); err != nil { // another node's slot: answered -MOVED, not served
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	slot := int(redis.Slot(a))
	moveEmptySlot(ctx, t, from, to, aID, bID, slot)
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer ccancel()
		moveEmptySlot(cctx, t, to, from, bID, aID, slot)
	}()
	if !r3Deliver(ctx, t, cc, rc, a, "a") {
		t.Error("the first channel is not delivered after its slot moved")
	}
	if !r3Deliver(ctx, t, cc, rc, b, "b") {
		t.Error("the second channel is not delivered although the PubSub is now on the node that owns its slot")
	}
}

// A shard call across slots is refused the way the cluster refuses it, and
// nothing is subscribed by it: before, it was sent, answered -CROSSSLOT (dropped
// in pubsub mode) and the channels were silently dead.
func TestClusterSSubscribeAcrossSlotsReturnsErrCrossSlot(t *testing.T) {
	cc := newClusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const a, b = "{l1204-a}.x", "{l1204-b}.y"
	if redis.Slot(a) == redis.Slot(b) {
		t.Fatal("test setup: same slot")
	}
	if ps, err := cc.SSubscribe(ctx, a, b); !errors.Is(err, redis.ErrCrossSlot) || ps != nil {
		if ps != nil {
			_ = ps.Close()
		}
		t.Fatalf("ClusterClient.SSubscribe across slots = (%v, %v), want (nil, ErrCrossSlot)", ps, err)
	}
	ps, err := cc.SSubscribe(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ps.Close() }()
	rc := r1Collect(ps)
	time.Sleep(300 * time.Millisecond)
	const sameSlot = "{l1204-a}.q"
	if err := ps.SSubscribe(ctx, sameSlot, b); !errors.Is(err, redis.ErrCrossSlot) {
		t.Fatalf("PubSub.SSubscribe across slots = %v, want ErrCrossSlot", err)
	}
	if !r3Deliver(ctx, t, cc, rc, a, "a") {
		t.Error("the subscribed channel stopped delivering after a refused call")
	}
	for i := 0; i < 3; i++ {
		_, _ = cc.SPublish(ctx, sameSlot, fmt.Sprintf("q-%d", i))
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if n := r3Delivered(rc, sameSlot, "q", 3); n != 0 {
		t.Errorf("%d messages of a channel from the refused call", n)
	}
}

// SUnsubscribe across slots: the cluster refuses it as one command
// (-CROSSSLOT), so the PubSub sends one per slot, and the channels it removed
// stop delivering while the one it kept goes on.
func TestClusterSUnsubscribeAcrossSlotsRemovesBoth(t *testing.T) {
	addrs := clusterAddrsFromEnv(t)
	cc := newClusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	seed, err := redis.NewClient(addrs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = seed.Close() }()
	const a, keep = "{l1204-u}.x", "{l1204-u}.keep"
	ownerAddr, _, _, _ := clusterOwnerOf(ctx, t, seed, int(redis.Slot(a)))
	b := r3ChannelOn(ctx, t, seed, a, ownerAddr, true)

	ps, err := cc.SSubscribe(ctx, a, keep)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ps.Close() }()
	rc := r1Collect(ps)
	if err := ps.SSubscribe(ctx, b); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	for _, ch := range []string{a, keep, b} {
		if !r3Deliver(ctx, t, cc, rc, ch, "before") {
			t.Fatalf("%s is not delivered before the SUnsubscribe", ch)
		}
	}
	if err := ps.SUnsubscribe(ctx, a, b); err != nil {
		t.Fatalf("SUnsubscribe across slots: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	for i := 0; i < 5; i++ {
		_, _ = cc.SPublish(ctx, a, fmt.Sprintf("after-%d", i))
		_, _ = cc.SPublish(ctx, b, fmt.Sprintf("after-%d", i))
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	for _, ch := range []string{a, b} {
		if n := r3Delivered(rc, ch, "after", 5); n != 0 {
			t.Errorf("%s: %d messages after the SUnsubscribe, want none (the server refused the cross-slot command and the channel stayed)", ch, n)
		}
	}
	if !r3Deliver(ctx, t, cc, rc, keep, "kept") {
		t.Error("the channel that was not removed stopped delivering")
	}
}
