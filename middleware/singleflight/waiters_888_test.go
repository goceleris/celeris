package singleflight

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// serve888 runs one GET /w through chain in a goroutine and returns a channel
// that delivers its outcome. A panic in the chain is recovered and reported.
type outcome888 struct {
	status   int
	body     string
	hit      string
	err      error
	panicked any
}

func serve888(chain ...celeris.HandlerFunc) <-chan outcome888 {
	out := make(chan outcome888, 1)
	go func() {
		c, rec := celeristest.NewContext("GET", "/w", celeristest.WithHandlers(chain...))
		var o outcome888
		func() {
			defer func() { o.panicked = recover() }()
			o.err = c.Next()
		}()
		o.status, o.body, o.hit = rec.StatusCode, string(rec.Body), rec.Header("x-singleflight")
		celeristest.ReleaseContext(c)
		out <- o
	}()
	return out
}

func await888(t *testing.T, who string, ch <-chan outcome888) outcome888 {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: no answer within 5 s", who)
		return outcome888{}
	}
}

// joinHook888 counts the waiters that join; it returns a channel with one
// value per join.
func joinHook888(t *testing.T) <-chan struct{} {
	t.Helper()
	joined := make(chan struct{}, 64)
	prev := testHookWaiterJoined
	testHookWaiterJoined = func() { joined <- struct{}{} }
	t.Cleanup(func() { testHookWaiterJoined = prev })
	return joined
}

func withCtx888(ctx context.Context) celeris.HandlerFunc {
	return func(c *celeris.Context) error { c.SetContext(ctx); return c.Next() }
}

func settledGoroutines888() int {
	most := 0
	for range 20 {
		if n := runtime.NumGoroutine(); n > most {
			most = n
		}
		time.Sleep(5 * time.Millisecond)
	}
	return most
}

// TestWaitersStartNoGoroutine888: a waiter raced the leader's WaitGroup
// against its request context in a goroutine of its own, so N waiters held
// 2N goroutines for as long as the leader ran. The call has one done channel,
// made when the first waiter joins and closed by the leader, which a waiter
// selects on directly.
func TestWaitersStartNoGoroutine888(t *testing.T) {
	const n = 32
	joined := joinHook888(t)
	mw := New()
	entered, gate := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	h := func(c *celeris.Context) error {
		if runs.Add(1) == 1 {
			close(entered)
			<-gate
		}
		return c.String(200, "value")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leader := serve888(mw, h)
	<-entered

	before := settledGoroutines888()
	outs := make([]<-chan outcome888, n)
	for i := range outs {
		outs[i] = serve888(withCtx888(ctx), mw, h)
	}
	for range n {
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Fatal("not every waiter joined the leader")
		}
	}
	during := settledGoroutines888()
	t.Logf("goroutines: %d before, %d with %d waiters blocked", before, during, n)
	if extra := during - before; extra > n {
		t.Errorf("%d waiters added %d goroutines, want %d (the waiters themselves)", n, extra, n)
	}
	close(gate)
	if o := await888(t, "leader", leader); o.status != 200 {
		t.Errorf("leader: %d", o.status)
	}
	for i, ch := range outs {
		if o := await888(t, fmt.Sprintf("waiter %d", i), ch); o.status != 200 || o.body != "value" || o.hit != "HIT" {
			t.Errorf("waiter %d: %d %q x-singleflight %q, want 200 \"value\" HIT", i, o.status, o.body, o.hit)
		}
	}
	if r := runs.Load(); r != 1 {
		t.Errorf("handler ran %d times, want 1", r)
	}
}

// TestWaiterLeavesWithItsContextWhileTheLeaderRuns888: the waiter's context
// ending still releases it before the leader returns (the reason the wait
// selects on the context at all: a timeout middleware in front).
func TestWaiterLeavesWithItsContextWhileTheLeaderRuns888(t *testing.T) {
	joined := joinHook888(t)
	mw := New()
	entered, gate := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	h := func(c *celeris.Context) error {
		if runs.Add(1) == 1 {
			close(entered)
			<-gate
		}
		return c.String(200, "value")
	}
	leader := serve888(mw, h)
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	waiter := serve888(withCtx888(ctx), mw, h)
	<-joined
	cancel()
	if o := await888(t, "waiter", waiter); !errors.Is(o.err, context.Canceled) {
		t.Errorf("waiter: err=%v, want context.Canceled", o.err)
	}
	close(gate)
	if o := await888(t, "leader", leader); o.status != 200 {
		t.Errorf("leader: %d", o.status)
	}
}

// TestWaitersLeavingAsTheLeaderPublishes888 (run with -race): waiters whose
// contexts end while the leader finishes get either the response or their
// context's error, and nothing hangs; the done channel is made by a waiter
// under the group's lock and closed by the leader.
func TestWaitersLeavingAsTheLeaderPublishes888(t *testing.T) {
	for i := range 100 {
		mw := New()
		entered, gate := make(chan struct{}), make(chan struct{})
		var runs atomic.Int32
		h := func(c *celeris.Context) error {
			if runs.Add(1) == 1 {
				close(entered)
				<-gate
			}
			return c.String(200, "value")
		}
		leader := serve888(mw, h)
		<-entered
		var wg sync.WaitGroup
		for j := range 4 {
			ctx, cancel := context.WithCancel(context.Background())
			ch := serve888(withCtx888(ctx), mw, h)
			wg.Go(func() {
				defer cancel()
				if j%2 == 0 {
					go func() { runtime.Gosched(); cancel() }()
				}
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Errorf("round %d: a waiter did not return", i)
				}
			})
		}
		time.Sleep(time.Millisecond)
		close(gate)
		wg.Wait()
		await888(t, "leader", leader)
	}
}

// TestCanceledLeaderDoesNotFailItsWaiters888: the leader's client goes away
// while its handler runs: the request context ends and the handler returns
// that error. The waiters' clients are still there, so a waiter runs its own
// handler instead of returning the leader's cancellation (celeris#888 item
// 26, from #939 review r2). A leader that failed for another reason hands
// that error on, as before, and so does a leader that panicked.
func TestCanceledLeaderDoesNotFailItsWaiters888(t *testing.T) {
	errUpstream := errors.New("upstream is down")
	for _, tc := range []struct {
		name       string
		how        string // how the leader's handler ends
		wantRuns   int32
		wantWaiter string
	}{
		// The defect: the waiters answered the leader's context.Canceled.
		{"leader-context-canceled", "cancel", 3, "200 value"},
		{"leader-deadline-exceeded", "deadline", 3, "200 value"},
		// Controls.
		{"leader-failed-otherwise", "error", 1, "error"},
		{"leader-canceled-but-failed-otherwise", "cancel-other", 1, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joined := joinHook888(t)
			var lctx context.Context
			var cancel context.CancelFunc
			if tc.how == "deadline" {
				lctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
			} else {
				lctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			mw := New()
			entered, gate := make(chan struct{}), make(chan struct{})
			var runs atomic.Int32
			h := func(c *celeris.Context) error {
				if runs.Add(1) == 1 {
					close(entered)
					<-gate
					switch tc.how {
					case "cancel", "deadline":
						if tc.how == "cancel" {
							cancel()
						} // the deadline arm ends at its deadline
						<-lctx.Done()
						return fmt.Errorf("upstream call: %w", lctx.Err())
					case "cancel-other":
						cancel()
						return errUpstream
					default:
						return errUpstream
					}
				}
				return c.String(200, "value")
			}
			leader := serve888(withCtx888(lctx), mw, h)
			<-entered
			const waiters = 2
			outs := make([]<-chan outcome888, waiters)
			for i := range outs {
				outs[i] = serve888(mw, h)
			}
			for range waiters {
				<-joined
			}
			close(gate)

			l := await888(t, "leader", leader)
			if tc.wantWaiter == "200 value" && l.err == nil {
				t.Errorf("leader: no error, want its own context error")
			}
			for i, ch := range outs {
				o := await888(t, fmt.Sprintf("waiter %d", i), ch)
				got := fmt.Sprintf("%d %s", o.status, o.body)
				if o.err != nil {
					got = "error"
				}
				if o.panicked != nil {
					got = fmt.Sprint("panic ", o.panicked)
				}
				if got != tc.wantWaiter {
					t.Errorf("waiter %d: %s (err %v), want %s", i, got, o.err, tc.wantWaiter)
				}
				if tc.wantWaiter == "error" && (o.err == nil || o.err.Error() != errUpstream.Error()) {
					t.Errorf("waiter %d: err=%v, want the leader's %q", i, o.err, errUpstream)
				}
			}
			if r := runs.Load(); r != tc.wantRuns {
				t.Errorf("handler ran %d times, want %d", r, tc.wantRuns)
			}
		})
	}
}
