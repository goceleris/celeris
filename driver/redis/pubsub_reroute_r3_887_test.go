package redis

// Review round 3 of celeris#887 item 31 (PR #1154): a mutant that survived
// round 2's suite (the sunsubscribe handler using the cached served slot), and
// the failing flag's state machine.

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// m1. The served slot is re-selected when its channels are gone: with {a}x and
// foo on the same node, SUnsubscribe({a}x) leaves foo as the only channel, and
// the move of foo's slot must be followed. A handler that read the cached served
// slot instead of re-deriving it would ignore foo's sunsubscribe, and the
// subscription would be lost until some other reroute.
func TestClusterServedSlotIsReselectedWhenItsChannelsAreGone887(t *testing.T) {
	cl := newRPCluster(t, 3)
	cc := newRPClusterClient(t, cl)
	const a, f = "{a}x", "foo" // slots 15495 and 12182: both on nodes[2]
	holder := cl.owner(int(Slot(a)))
	if holder != cl.owner(int(Slot(f))) {
		t.Fatal("setup: the two slots are on different nodes")
	}
	ps, err := cc.SSubscribe(rpCtx(t), a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	rc := collect(ps)
	poll(t, "SSUBSCRIBE on the owner", 5*time.Second, func() bool { return holder.subscribers("ssub") == 1 })
	if err := ps.SSubscribe(rpCtx(t), f); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := ps.SUnsubscribe(rpCtx(t), a); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	to := cl.nodes[0]
	cl.moveSlot(int(Slot(f)), to, 0)
	poll(t, "the remaining channel followed to its new owner", 10*time.Second, func() bool { return to.subscribers("ssub") == 1 })
	cl.spublish(f, "f1")
	poll(t, "f1", 3*time.Second, func() bool { return rc.count("f1") == 1 })
}

// The failing flag, sequentially: a PubSub success undoes a PubSub failure and
// nothing else.
func TestFailFlagStateMachine887(t *testing.T) {
	var f failFlag
	f.markPubSub()
	if !f.Load() {
		t.Fatal("markPubSub did not set the flag")
	}
	f.clearPubSub()
	if f.Load() {
		t.Fatal("clearPubSub did not undo markPubSub")
	}
	f.Store(true) // the command path
	f.clearPubSub()
	if !f.Load() {
		t.Fatal("clearPubSub undid the command path's mark")
	}
	f.markPubSub()
	f.Store(true) // the command path takes the node over
	f.clearPubSub()
	if !f.Load() {
		t.Fatal("clearPubSub undid a mark the command path set after the PubSub's")
	}
	f.markPubSub()
	f.Store(false) // a topology probe found it alive
	f.clearPubSub()
	f.Store(true)
	f.clearPubSub()
	if !f.Load() {
		t.Fatal("a stale PubSub mark survived a Store(false) and made a later command mark clearable")
	}
}

// The flag under concurrency. A command's Store(true) and a PubSub success that
// run at the same time leave the flag set whatever their order (Store first:
// the success finds a mark that is not its own; the success first: Store sets
// it afterwards). A clear that read the PubSub's mark, was overtaken by the
// command's whole Store, and then wrote false would erase the command's fresh
// mark.
func TestFailFlagCommandMarkIsNotLostToAConcurrentPubSubSuccess887(t *testing.T) {
	const rounds = 300000
	var f failFlag
	var start atomic.Int32
	var lost atomic.Int64
	var wg sync.WaitGroup
	round := make(chan struct{})
	done := make(chan struct{}, 2)
	wg.Add(2)
	run := func(op func()) {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			<-round
			for start.Load() == 0 {
				runtime.Gosched()
			}
			op()
			done <- struct{}{}
		}
	}
	go run(func() { f.clearPubSub() })
	go run(func() { f.Store(true) })
	for i := 0; i < rounds; i++ {
		f.markPubSub()
		start.Store(0)
		round <- struct{}{}
		round <- struct{}{}
		start.Store(1)
		<-done
		<-done
		if !f.Load() {
			lost.Add(1)
		}
	}
	wg.Wait()
	if n := lost.Load(); n != 0 {
		t.Errorf("%d of %d rounds lost the command path's mark to a concurrent PubSub success", n, rounds)
	}
}

func BenchmarkFailFlagStore887(b *testing.B) {
	var f failFlag
	b.ReportAllocs()
	for b.Loop() {
		f.Store(false)
	}
}

func BenchmarkAnyNode887(b *testing.B) {
	c := &ClusterClient{nodes: map[string]*clusterNode{}}
	for i := 0; i < 6; i++ {
		c.nodes[string(rune('a'+i))] = &clusterNode{}
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = c.anyNode()
	}
}
