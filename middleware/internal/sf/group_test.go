package sf

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// joinHook makes TestHookFollowerJoined report each follower that joins.
func joinHook(t *testing.T) <-chan struct{} {
	t.Helper()
	joined := make(chan struct{}, 16)
	prev := TestHookFollowerJoined
	TestHookFollowerJoined = func() { joined <- struct{}{} }
	t.Cleanup(func() { TestHookFollowerJoined = prev })
	return joined
}

type result struct {
	v      int
	leader bool
	err    error
}

func goDo(ctx context.Context, g *Group[int], key string, fn func() (int, error), then func(int, error)) <-chan result {
	out := make(chan result, 1)
	go func() {
		v, leader, err := g.Do(ctx, key, fn, then)
		out <- result{v, leader, err}
	}()
	return out
}

func within(t *testing.T, who string, ch <-chan result, d time.Duration) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(d):
		t.Fatalf("%s: Do did not return within %v", who, d)
		return result{}
	}
}

// TestDoReleasesTheKeyWhenTheLeaderPanics921: celeris#921. A panic in fn (or
// in then, before or after the result is published) used to leave the key's
// call in the map with its WaitGroup never released, so its followers, and
// every later caller for the key, waited forever. The panic must reach the
// leader unchanged, a follower that had no result yet must get
// ErrLeaderPanicked, and the next Do for the key must run its own fn.
func TestDoReleasesTheKeyWhenTheLeaderPanics921(t *testing.T) {
	for _, where := range []string{"fn", "then"} {
		t.Run(where, func(t *testing.T) {
			joined := joinHook(t)
			g := New[int]()
			entered, gate := make(chan struct{}), make(chan struct{})
			fn := func() (int, error) {
				close(entered)
				<-gate
				if where == "fn" {
					panic("leader panicked")
				}
				return 7, nil
			}
			var then func(int, error)
			if where == "then" {
				then = func(int, error) { panic("leader panicked") }
			}
			leaderPanic := make(chan any, 1)
			go func() {
				defer func() { leaderPanic <- recover() }()
				_, _, _ = g.Do(context.Background(), "k", fn, then)
			}()
			<-entered
			follower := goDo(context.Background(), g, "k", func() (int, error) { return 0, errors.New("the follower ran fn") }, nil)
			<-joined
			close(gate)
			if p := <-leaderPanic; p != "leader panicked" {
				t.Errorf("leader: recovered %v, want its own panic", p)
			}
			f := within(t, "follower", follower, 3*time.Second)
			switch where {
			case "fn":
				if f.leader || !errors.Is(f.err, ErrLeaderPanicked) {
					t.Errorf("follower: leader=%v err=%v, want ErrLeaderPanicked", f.leader, f.err)
				}
			case "then":
				// The result was published before then ran.
				if f.leader || f.err != nil || f.v != 7 {
					t.Errorf("follower: %d leader=%v err=%v, want the published 7", f.v, f.leader, f.err)
				}
			}
			next := within(t, "next caller", goDo(context.Background(), g, "k", func() (int, error) { return 9, nil }, nil), 3*time.Second)
			if !next.leader || next.v != 9 || next.err != nil {
				t.Errorf("next caller: %d leader=%v err=%v, want to lead its own call (9)", next.v, next.leader, next.err)
			}
		})
	}
}

// TestDoReleasesFollowersBeforeThen921: fn's result is published before then
// runs. A follower that joined during fn and one that joins during then both
// get it while then is still running, and fn runs once; after then returns
// the key is free.
func TestDoReleasesFollowersBeforeThen921(t *testing.T) {
	joined := joinHook(t)
	g := New[int]()
	entered, gate := make(chan struct{}), make(chan struct{})
	inThen, thenGate := make(chan struct{}), make(chan struct{})
	var runs int
	var mu sync.Mutex
	fn := func() (int, error) {
		mu.Lock()
		runs++
		mu.Unlock()
		close(entered)
		<-gate
		return 7, nil
	}
	then := func(v int, err error) {
		close(inThen)
		<-thenGate
	}
	leader := goDo(context.Background(), g, "k", fn, then)
	<-entered
	during := goDo(context.Background(), g, "k", fn, nil)
	<-joined
	close(gate)
	<-inThen
	inside := goDo(context.Background(), g, "k", fn, nil)
	for who, ch := range map[string]<-chan result{"follower that joined during fn": during, "caller that came during then": inside} {
		r := within(t, who, ch, 3*time.Second)
		if r.leader || r.v != 7 || r.err != nil {
			t.Errorf("%s: %d leader=%v err=%v, want the published 7", who, r.v, r.leader, r.err)
		}
	}
	close(thenGate)
	if l := within(t, "leader", leader, 3*time.Second); !l.leader || l.v != 7 {
		t.Errorf("leader: %d leader=%v", l.v, l.leader)
	}
	if runs != 1 {
		t.Errorf("fn ran %d times, want 1", runs)
	}
	next := within(t, "next caller", goDo(context.Background(), g, "k", func() (int, error) { return 9, nil }, nil), 3*time.Second)
	if !next.leader || next.v != 9 {
		t.Errorf("next caller after then: %d leader=%v, want to lead its own call", next.v, next.leader)
	}
}

// TestDoFollowerWaitEndsWithItsContext921: a follower waits only as long as
// its context lives.
func TestDoFollowerWaitEndsWithItsContext921(t *testing.T) {
	joined := joinHook(t)
	g := New[int]()
	entered, gate := make(chan struct{}), make(chan struct{})
	defer close(gate)
	_ = goDo(context.Background(), g, "k", func() (int, error) { close(entered); <-gate; return 7, nil }, nil)
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	f := goDo(ctx, g, "k", func() (int, error) { return 0, nil }, nil)
	<-joined
	cancel()
	r := within(t, "follower", f, 3*time.Second)
	if r.leader || !errors.Is(r.err, context.Canceled) {
		t.Errorf("follower: leader=%v err=%v, want context.Canceled", r.leader, r.err)
	}
}
