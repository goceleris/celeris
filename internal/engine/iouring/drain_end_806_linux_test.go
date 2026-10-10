//go:build linux

package iouring

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#806: the send drain after a cancel ends with the budget of the last
// Engine.Shutdown, as epoll's does, and the loop's waits are bounded while it
// runs. drainEnd is that end, shared with the HTTP/2 wait.

func budgetWorker806(t *testing.T, budget func() (context.Context, context.CancelFunc), wt time.Duration) *Worker {
	t.Helper()
	w := &Worker{cfg: resource.Config{WriteTimeout: wt}}
	if budget != nil {
		ctx, cancel := budget()
		t.Cleanup(cancel)
		w.drainBudget = &atomic.Pointer[context.Context]{}
		w.drainBudget.Store(&ctx)
	}
	return w
}

func TestDrainEnd806(t *testing.T) {
	const start = int64(1_000_000_000_000)
	floor := shutdownSendDrainNanos
	in := func(d time.Duration) func() (context.Context, context.CancelFunc) {
		return func() (context.Context, context.CancelFunc) {
			// The deadline is absolute wall-clock time; the cases are
			// worked out against it with start moved to the same clock.
			return context.WithTimeout(context.Background(), d)
		}
	}
	cancelled := func() (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx, cancel
	}
	live := func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }

	t.Run("no-budget-handed-over", func(t *testing.T) {
		w := &Worker{}
		if end, bounded := w.drainEnd(start, floor); !bounded || end != start+floor {
			t.Fatalf("end=%d bounded=%v, want the floor %d", end, bounded, start+floor)
		}
	})
	t.Run("budget-cell-empty", func(t *testing.T) {
		w := &Worker{drainBudget: &atomic.Pointer[context.Context]{}}
		if end, bounded := w.drainEnd(start, floor); !bounded || end != start+floor {
			t.Fatalf("end=%d bounded=%v, want the floor %d", end, bounded, start+floor)
		}
	})
	t.Run("budget-done-gets-the-floor", func(t *testing.T) {
		w := budgetWorker806(t, cancelled, 30*time.Second)
		if end, bounded := w.drainEnd(start, floor); !bounded || end != start+floor {
			t.Fatalf("end=%d bounded=%v, want the floor %d", end, bounded, start+floor)
		}
	})
	t.Run("live-deadline-extends-the-drain", func(t *testing.T) {
		w := budgetWorker806(t, in(30*time.Second), 0)
		p := w.drainBudget.Load()
		want, _ := (*p).Deadline()
		end, bounded := w.drainEnd(start, floor)
		if !bounded || end != want.UnixNano() {
			t.Fatalf("end=%d bounded=%v, want the budget's deadline %d", end, bounded, want.UnixNano())
		}
	})
	t.Run("deadline-inside-the-floor-gets-the-floor", func(t *testing.T) {
		// The deadline is 10 ms away on the real clock, and start is taken
		// from it, so the floor is 240 ms past it.
		w := budgetWorker806(t, in(10*time.Millisecond), 0)
		p := w.drainBudget.Load()
		dl, _ := (*p).Deadline()
		s := dl.UnixNano() - int64(10*time.Millisecond)
		end, bounded := w.drainEnd(s, floor)
		if !bounded || end != s+floor {
			t.Fatalf("end=%d bounded=%v, want the floor %d", end, bounded, s+floor)
		}
	})
	t.Run("writetimeout-caps-a-far-deadline", func(t *testing.T) {
		w := budgetWorker806(t, in(time.Hour), 2*time.Second)
		end, bounded := w.drainEnd(start, floor)
		if !bounded || end != start+int64(2*time.Second) {
			t.Fatalf("end=%d bounded=%v, want start+WriteTimeout %d", end, bounded, start+int64(2*time.Second))
		}
	})
	t.Run("no-deadline-no-writetimeout-waits-for-the-budget", func(t *testing.T) {
		w := budgetWorker806(t, live, 0)
		if _, bounded := w.drainEnd(start, floor); bounded {
			t.Fatal("a live ctx with no deadline and no WriteTimeout is bounded: want 'until the budget is done'")
		}
	})
	t.Run("no-deadline-writetimeout-bounds-it", func(t *testing.T) {
		w := budgetWorker806(t, live, 3*time.Second)
		end, bounded := w.drainEnd(start, floor)
		if !bounded || end != start+int64(3*time.Second) {
			t.Fatalf("end=%d bounded=%v, want start+WriteTimeout %d", end, bounded, start+int64(3*time.Second))
		}
	})
	t.Run("budget-arrives-late", func(t *testing.T) {
		// StartWithContext's cancel reaches the worker before the watcher's
		// Engine.Shutdown stores the budget: the end moves out when it does.
		w := &Worker{drainBudget: &atomic.Pointer[context.Context]{}}
		before, _ := w.drainEnd(start, floor)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		t.Cleanup(cancel)
		w.drainBudget.Store(&ctx)
		after, bounded := w.drainEnd(start, floor)
		want, _ := ctx.Deadline()
		if !bounded || before != start+floor || after != want.UnixNano() {
			t.Fatalf("before=%d after=%d bounded=%v, want %d then %d", before, after, bounded, start+floor, want.UnixNano())
		}
	})
}

// TestAdaptiveTimeoutIsCappedWhileDraining806: a worker whose context is
// cancelled looks at the end of its drain between waits, so no wait may be
// longer than h2PoolDrainPoll, whatever the idle backoff would have given
// (a worker with no listen socket, which the drain leaves it after
// stopAccepting, would otherwise wait a second).
func TestAdaptiveTimeoutIsCappedWhileDraining806(t *testing.T) {
	for _, tc := range []struct {
		name     string
		draining bool
		want     time.Duration
	}{
		{"running", false, time.Second},
		{"draining", true, h2PoolDrainPoll},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &Worker{listenFD: -1, draining: tc.draining}
			if got := w.adaptiveTimeout(); got != tc.want {
				t.Fatalf("adaptiveTimeout = %v, want %v", got, tc.want)
			}
		})
	}
}
