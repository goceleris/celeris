package sf

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errNotFound921 = errors.New("not found: /items/42")

// doErr runs one Do for key with fn and no then.
func doErr(g *Group[[]byte], key string, fn func() ([]byte, error)) ([]byte, bool, error) {
	return g.Do(context.Background(), key, noLimit(fn), nil)
}

// TestDoLeaderErrorWithoutFollowersAllocatesNothing: the followers get the
// leader's error as a copy (handoff.Error), but a leader that has no follower
// must not pay for that copy. A cached route that answers its misses with a
// returned error (a 404 for an unknown id) takes this path on every miss.
func TestDoLeaderErrorWithoutFollowersAllocatesNothing(t *testing.T) {
	g := New[[]byte]()
	// The adapter to Do's shape is made once, outside the measured call.
	fn := noLimit(func() ([]byte, error) { return nil, errNotFound921 })
	for range 100 {
		_, _, _ = g.Do(context.Background(), "k", fn, nil)
	}
	allocs := testing.AllocsPerRun(1000, func() {
		if _, leader, err := g.Do(context.Background(), "k", fn, nil); !leader || err != errNotFound921 {
			t.Fatalf("leader=%v err=%v, want the leader with fn's own error", leader, err)
		}
	})
	t.Logf("leader error, no follower: %.1f allocs/op", allocs)
	if allocs != 0 {
		t.Errorf("a leader whose fn returned an error, with no follower, made %.1f allocations per call, want 0", allocs)
	}
}

// TestDoLeaderErrorReachesItsFollowers: a follower that joined while fn ran
// gets fn's error (a copy with the same message), and the key is free once
// the leader returns.
func TestDoLeaderErrorReachesItsFollowers(t *testing.T) {
	joined := joinHook(t)
	g := New[[]byte]()
	entered, gate := make(chan struct{}), make(chan struct{})
	type res struct {
		leader bool
		err    error
	}
	leaderOut := make(chan res, 1)
	go func() {
		_, leader, err := doErr(g, "k", func() ([]byte, error) { close(entered); <-gate; return nil, errNotFound921 })
		leaderOut <- res{leader, err}
	}()
	<-entered
	followerOut := make(chan res, 1)
	go func() {
		_, leader, err := doErr(g, "k", func() ([]byte, error) { return nil, errors.New("the follower ran fn") })
		followerOut <- res{leader, err}
	}()
	<-joined
	close(gate)
	for who, ch := range map[string]chan res{"leader": leaderOut, "follower": followerOut} {
		select {
		case r := <-ch:
			if r.leader != (who == "leader") || r.err == nil || r.err.Error() != errNotFound921.Error() {
				t.Errorf("%s: leader=%v err=%v, want fn's error %q", who, r.leader, r.err, errNotFound921)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: Do did not return", who)
		}
	}
	if _, leader, err := doErr(g, "k", func() ([]byte, error) { return []byte("x"), nil }); !leader || err != nil {
		t.Errorf("next caller: leader=%v err=%v, want to lead its own call", leader, err)
	}
}

// TestDoExpiredResultIsNotHandedOut921: while then holds the key, a caller
// that arrives within the result's freshness takes the published result; one
// that arrives after it leads a call of its own. The stale call's release
// must not remove the newer call from the map: a caller that comes while the
// newer call runs joins it.
func TestDoExpiredResultIsNotHandedOut921(t *testing.T) {
	joined := joinHook(t)
	g := New[int]()
	// The caller within the freshness must reach Do before it ends, which a
	// GC pause or a loaded -race runner can delay, so it is generous; the
	// caller after it only needs the sleep below, which outlasts it.
	const fresh = 500 * time.Millisecond
	inThen, thenGate := make(chan struct{}), make(chan struct{})
	first := make(chan result, 1)
	go func() {
		v, leader, err := g.Do(context.Background(), "k",
			func() (int, time.Duration, error) { return 1, fresh, nil },
			func(int, error) { close(inThen); <-thenGate })
		first <- result{v, leader, err}
	}()
	<-inThen
	if r := within(t, "caller within the freshness", goDo(context.Background(), g, "k", func() (int, error) { return 2, nil }, nil), 3*time.Second); r.leader || r.v != 1 {
		t.Errorf("caller within the freshness: %d leader=%v, want the published 1", r.v, r.leader)
	}
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		close(thenGate)
		t.Fatal("the caller within the freshness never joined the published call")
	}
	time.Sleep(2 * fresh)

	entered, gate := make(chan struct{}), make(chan struct{})
	second := goDo(context.Background(), g, "k", func() (int, error) { close(entered); <-gate; return 3, nil }, nil)
	select {
	case <-entered:
	case r := <-second:
		close(thenGate)
		t.Fatalf("a caller after the freshness took the stale call's result: %d leader=%v, want to lead its own call", r.v, r.leader)
	case <-time.After(3 * time.Second):
		t.Fatal("the caller after the freshness neither ran fn nor returned")
	}
	// The stale call's then returns while the newer call still runs.
	close(thenGate)
	if r := within(t, "first leader", first, 3*time.Second); !r.leader || r.v != 1 {
		t.Errorf("first leader: %d leader=%v", r.v, r.leader)
	}
	third := goDo(context.Background(), g, "k", func() (int, error) { return 4, nil }, nil)
	select {
	case <-joined:
	case r := <-third:
		t.Fatalf("a caller during the newer call did not join it: %d leader=%v (the stale call's release removed the newer call)", r.v, r.leader)
	case <-time.After(3 * time.Second):
		t.Fatal("the third caller neither joined nor returned")
	}
	close(gate)
	if r := within(t, "caller after the freshness", second, 3*time.Second); !r.leader || r.v != 3 {
		t.Errorf("caller after the freshness: %d leader=%v, want to lead its own call (3)", r.v, r.leader)
	}
	if r := within(t, "third", third, 3*time.Second); r.leader || r.v != 3 {
		t.Errorf("third: %d leader=%v, want the newer call's 3", r.v, r.leader)
	}
}
