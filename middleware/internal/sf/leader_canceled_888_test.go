package sf

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestDoCanceledLeaderErrorIsNotHandedToFollowers888: a leader whose own
// request context ended (its client went away, its deadline passed) returns
// an error that says so. Handing that error to the followers fails requests
// whose clients are still there, so a follower gets a distinct error and
// runs its own handler (celeris#888 item 26, from #940 review r2).
func TestDoCanceledLeaderErrorIsNotHandedToFollowers888(t *testing.T) {
	for _, tc := range []struct {
		name string
		mk   func() (context.Context, context.CancelFunc)
		want error
	}{
		{"canceled", func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, context.Canceled},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		}, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joined := joinHook(t)
			g := New[int]()
			lctx, cancel := tc.mk()
			defer cancel()
			entered, gate := make(chan struct{}), make(chan struct{})
			leader := make(chan result, 1)
			go func() {
				v, l, err := g.Do(lctx, "k", func() (int, time.Duration, error) {
					close(entered)
					<-gate
					cancel()
					return 0, 0, fmt.Errorf("handler: %w", lctx.Err())
				}, nil)
				leader <- result{v, l, err}
			}()
			<-entered
			follower := goDo(context.Background(), g, "k", func() (int, error) { return 0, nil }, nil)
			<-joined
			close(gate)

			if r := within(t, "leader", leader, 3*time.Second); !r.leader || !errors.Is(r.err, tc.want) {
				t.Errorf("leader: leader=%v err=%v, want its own %v", r.leader, r.err, tc.want)
			}
			r := within(t, "follower", follower, 3*time.Second)
			if r.err == nil || errors.Is(r.err, tc.want) {
				t.Errorf("follower: err=%v, want an error that is not the leader's %v", r.err, tc.want)
			}
		})
	}
}

// TestDoLeaderErrorUnrelatedToItsContextStillReachesFollowers888: the control.
// A leader whose context is still live, or whose error is not its context's,
// hands the error on as before.
func TestDoLeaderErrorUnrelatedToItsContextStillReachesFollowers888(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cancel bool
		err    error
	}{
		{"live-context", false, errNotFound921},
		{"canceled-context-other-error", true, errNotFound921},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joined := joinHook(t)
			g := New[int]()
			lctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, gate := make(chan struct{}), make(chan struct{})
			leader := make(chan result, 1)
			go func() {
				v, l, err := g.Do(lctx, "k", func() (int, time.Duration, error) {
					close(entered)
					<-gate
					if tc.cancel {
						cancel()
					}
					return 0, 0, tc.err
				}, nil)
				leader <- result{v, l, err}
			}()
			<-entered
			follower := goDo(context.Background(), g, "k", func() (int, error) { return 0, nil }, nil)
			<-joined
			close(gate)
			within(t, "leader", leader, 3*time.Second)
			if r := within(t, "follower", follower, 3*time.Second); r.err == nil || r.err.Error() != tc.err.Error() {
				t.Errorf("follower: err=%v, want the leader's %q", r.err, tc.err)
			}
		})
	}
}
