package sf

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// settledGoroutines returns the largest goroutine count seen over a short
// window, so a goroutine the code under test starts just after a follower
// joined is counted too.
func settledGoroutines() int {
	most := 0
	for range 20 {
		if n := runtime.NumGoroutine(); n > most {
			most = n
		}
		time.Sleep(5 * time.Millisecond)
	}
	return most
}

// TestDoFollowerOfPublishedCallAllocatesNothing888: a caller that arrives
// while then still holds the key takes the published result. With a context
// that has a Done channel (std, HTTP/2, the timeout middleware) it used to
// start a goroutine and make a channel to select on the call's WaitGroup,
// for a call that was already over. A call has one done channel, closed at
// publish, and a follower of a published call needs no allocation at all.
func TestDoFollowerOfPublishedCallAllocatesNothing888(t *testing.T) {
	g := New[int]()
	inThen, gate := make(chan struct{}), make(chan struct{})
	first := goDo(context.Background(), g, "k", func() (int, error) { return 7, nil },
		func(int, error) { close(inThen); <-gate })
	<-inThen
	defer func() { close(gate); within(t, "leader", first, 3*time.Second) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The adapter is made once, outside the measured call; it never runs.
	fn := noLimit(func() (int, error) { return 0, errors.New("the follower ran fn") })
	do := func() {
		v, leader, err := g.Do(ctx, "k", fn, nil)
		if leader || err != nil || v != 7 {
			t.Fatalf("follower of the published call: %d leader=%v err=%v, want the published 7", v, leader, err)
		}
	}
	do()
	allocs := testing.AllocsPerRun(1000, do)
	t.Logf("follower of a published call, ctx with Done: %.1f allocs/op", allocs)
	if allocs != 0 {
		t.Errorf("a follower that joined a published call made %.1f allocations per Do, want 0", allocs)
	}
}

// TestDoWaitingFollowersStartNoGoroutine888: N followers with a cancellable
// context wait on a running leader. Each used to start a goroutine of its
// own to wait on the WaitGroup, so the process held 2N goroutines for them.
func TestDoWaitingFollowersStartNoGoroutine888(t *testing.T) {
	const n = 32
	joined := joinHook(t)
	g := New[int]()
	entered, gate := make(chan struct{}), make(chan struct{})
	leader := goDo(context.Background(), g, "k", func() (int, error) { close(entered); <-gate; return 5, nil }, nil)
	<-entered

	before := settledGoroutines()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	outs := make([]<-chan result, n)
	for i := range outs {
		outs[i] = goDo(ctx, g, "k", func() (int, error) { return 0, errors.New("a follower ran fn") }, nil)
	}
	for range n {
		<-joined
	}
	during := settledGoroutines()
	t.Logf("goroutines: %d before, %d with %d followers waiting", before, during, n)
	if extra := during - before; extra > n {
		t.Errorf("%d followers waiting added %d goroutines, want %d (the followers themselves)", n, extra, n)
	}
	close(gate)
	within(t, "leader", leader, 3*time.Second)
	for i, ch := range outs {
		if r := within(t, fmt.Sprintf("follower %d", i), ch, 3*time.Second); r.leader || r.err != nil || r.v != 5 {
			t.Errorf("follower %d: %d leader=%v err=%v, want the leader's 5", i, r.v, r.leader, r.err)
		}
	}
}

// TestDoFollowersLeavingAsTheLeaderPublishes888: followers whose contexts end
// at the moment the leader publishes get either the result or their context's
// error, never a hang, on the error and the success path alike (run with
// -race: the done channel is created by a follower and closed by the leader).
func TestDoFollowersLeavingAsTheLeaderPublishes888(t *testing.T) {
	for _, withErr := range []bool{false, true} {
		t.Run(fmt.Sprintf("leader-error=%v", withErr), func(t *testing.T) {
			g := New[int]()
			for i := range 300 {
				entered, gate := make(chan struct{}), make(chan struct{})
				key := fmt.Sprint("k", i)
				leader := make(chan result, 1)
				go func() {
					v, l, err := g.Do(context.Background(), key, func() (int, time.Duration, error) {
						close(entered)
						<-gate
						if withErr {
							return 0, 0, errors.New("backend down")
						}
						return 1, 0, nil
					}, nil)
					leader <- result{v, l, err}
				}()
				<-entered
				var wg sync.WaitGroup
				for j := range 4 {
					ctx, cancel := context.WithCancel(context.Background())
					wg.Go(func() {
						defer cancel()
						if j%2 == 0 {
							go func() { runtime.Gosched(); cancel() }()
						}
						_, _, _ = g.Do(ctx, key, func() (int, time.Duration, error) { return 2, 0, nil }, nil)
					})
				}
				close(gate)
				done := make(chan struct{})
				go func() { wg.Wait(); <-leader; close(done) }()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatalf("round %d: a follower or the leader did not return", i)
				}
			}
		})
	}
}
