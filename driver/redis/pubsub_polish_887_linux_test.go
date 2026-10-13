//go:build linux

package redis

// celeris#887 items 12, 14 and 15: PubSub constructor and reconnect polish.
// The rig, the write gate and the server-side drop are those of
// pubsub_ctlwrite_929_linux_test.go.

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/async"
)

// pubsubLogHas reports whether a server-side connection received a command
// line that starts with verb and contains every name.
func pubsubLogHas(r *pubsubRig, verb string, names ...string) bool {
	r.log.mu.Lock()
	defer r.log.mu.Unlock()
	for _, cmds := range r.log.per {
		for _, c := range cmds {
			if !strings.HasPrefix(c, verb+" ") {
				continue
			}
			all := true
			for _, n := range names {
				if !strings.Contains(c, n) {
					all = false
				}
			}
			if all {
				return true
			}
		}
	}
	return false
}

// setBeforeBindHook installs f as testHookBeforeBind. The caller resets it
// with testHookBeforeBind.Store(nil) when it is done.
func setBeforeBindHook(f func(conn *redisConn)) { testHookBeforeBind.Store(&f) }

// closePubSub closes ps and waits for its reconnect goroutine to finish, as
// the other rig tests do before they end.
func closePubSub(t *testing.T, ps *PubSub) {
	t.Helper()
	_ = ps.Close()
	waitFor(t, "the reconnect goroutine to finish", 10*time.Second, func() bool {
		ps.reconnectingMu.Lock()
		defer ps.reconnectingMu.Unlock()
		return !ps.reconnecting
	})
}

// (item 14) The pool hands out a conn, the server closes it right after the
// handshake, and the loop's teardown (onClose) runs before newPubSub has
// bound the PubSub's close hook: the hook is set on a conn that is already
// closed, never fires, and no reconnect starts. A control call then returns
// ErrPubSubConnDown for ever, with nothing to replay it.
func TestPubSubConnClosedBeforeBindStillReconnects887(t *testing.T) {
	eachRigEngine(t, testPubSubConnClosedBeforeBindStillReconnects887)
}

func testPubSubConnClosedBeforeBindStillReconnects887(t *testing.T, eng rigEngine) {
	r := newPubsubRig(t, eng)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var fired atomic.Bool
	setBeforeBindHook(func(c *redisConn) {
		if !fired.CompareAndSwap(false, true) {
			return
		}
		r.dropServerSide() // the peer hangs up: the worker tears the conn down
		waitFor(t, "the loop to tear the new conn down (onClose)", 5*time.Second, c.closed.Load)
	})
	t.Cleanup(func() { testHookBeforeBind.Store(nil) })

	ps, err := r.cl.Subscribe(ctx) // no names: the first control call is the caller's own
	if err != nil {
		t.Fatalf("Subscribe with no names: %v", err)
	}
	if !fired.Load() {
		t.Fatal("fixture: the before-bind hook did not run")
	}
	testHookBeforeBind.Store(nil)
	t.Cleanup(func() { closePubSub(t, ps) })

	err = ps.Subscribe(ctx, "news")
	if err != nil && !errors.Is(err, ErrPubSubConnDown) {
		t.Fatalf("Subscribe on the PubSub whose conn closed before it was bound: %v, want nil or ErrPubSubConnDown", err)
	}
	waitFor(t, "the server to see SUBSCRIBE news (the reconnect replays it)", 8*time.Second, func() bool {
		return pubsubLogHas(r, "SUBSCRIBE", "news")
	})
}

// (item 12) The first control call of a constructor finds the conn down (the
// loop forgot the descriptor: ErrUnknownFD, mapped to ErrPubSubConnDown). The
// documented contract is that the call "need not be repeated" because the
// reconnect replays the set, so the constructor keeps the PubSub and returns
// it with a nil error. The engine's teardown that follows (the server side
// goes away here) starts the reconnect, which replays the channels.
func TestConstructorKeepsThePubSubWhenTheFirstControlCallFindsTheConnDown887(t *testing.T) {
	eachRigEngine(t, testConstructorKeepsThePubSubWhenTheFirstControlCallFindsTheConnDown887)
}

func testConstructorKeepsThePubSubWhenTheFirstControlCallFindsTheConnDown887(t *testing.T, eng rigEngine) {
	for _, tc := range []struct {
		name string
		verb string
		open func(r *pubsubRig, ctx context.Context) (*PubSub, error)
	}{
		{"Client.Subscribe", "SUBSCRIBE", func(r *pubsubRig, ctx context.Context) (*PubSub, error) {
			return r.cl.Subscribe(ctx, "news")
		}},
		{"Client.PSubscribe", "PSUBSCRIBE", func(r *pubsubRig, ctx context.Context) (*PubSub, error) {
			return r.cl.PSubscribe(ctx, "news*")
		}},
		{"ClusterClient.SSubscribe", "SSUBSCRIBE", func(r *pubsubRig, ctx context.Context) (*PubSub, error) {
			cc := &ClusterClient{nodes: map[string]*clusterNode{"n": {addr: "n", client: r.cl, primary: true}}}
			return cc.SSubscribe(ctx, "news")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPubsubRig(t, eng)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			r.gate.armFailWith(hasCmd(tc.verb), ErrUnknownFD)
			ps, err := tc.open(r, ctx)
			if err != nil || ps == nil {
				t.Fatalf("%s whose first control call found the conn down returned (%v, %v), want a PubSub and a nil error: the reconnect replays the call", tc.name, ps, err)
			}
			t.Cleanup(func() { closePubSub(t, ps) })
			r.dropServerSide() // the teardown the forgotten descriptor stands for
			waitFor(t, "the reconnect to replay "+tc.verb, 10*time.Second, func() bool {
				return pubsubLogHas(r, tc.verb, "news")
			})
		})
	}
}

// (item 12, guard) Only ErrPubSubConnDown is kept: any other error of the
// first control call means the command was not sent and nothing replays it, so
// the constructor closes the PubSub (its conn is released) and returns
// (nil, err), as before. ErrQueueFull on a live conn is such an error. This passes on main as well;
// the mutant whose firstControl keeps the PubSub on every error is its
// control.
func TestConstructorClosesThePubSubOnAnyOtherFirstControlError887(t *testing.T) {
	eachRigEngine(t, testConstructorClosesThePubSubOnAnyOtherFirstControlError887)
}

func testConstructorClosesThePubSubOnAnyOtherFirstControlError887(t *testing.T, eng rigEngine) {
	for _, tc := range []struct {
		name string
		verb string
		open func(r *pubsubRig, ctx context.Context) (*PubSub, error)
	}{
		{"Client.Subscribe", "SUBSCRIBE", func(r *pubsubRig, ctx context.Context) (*PubSub, error) {
			return r.cl.Subscribe(ctx, "news")
		}},
		{"Client.PSubscribe", "PSUBSCRIBE", func(r *pubsubRig, ctx context.Context) (*PubSub, error) {
			return r.cl.PSubscribe(ctx, "news*")
		}},
		{"ClusterClient.SSubscribe", "SSUBSCRIBE", func(r *pubsubRig, ctx context.Context) (*PubSub, error) {
			cc := &ClusterClient{nodes: map[string]*clusterNode{"n": {addr: "n", client: r.cl, primary: true}}}
			return cc.SSubscribe(ctx, "news")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPubsubRig(t, eng)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			r.gate.armFailWith(hasCmd(tc.verb), ErrQueueFull)
			// The conn the constructor built, to see that it is released.
			var built atomic.Pointer[redisConn]
			setBeforeBindHook(func(c *redisConn) { built.CompareAndSwap(nil, c) })
			t.Cleanup(func() { testHookBeforeBind.Store(nil) })
			ps, err := tc.open(r, ctx)
			testHookBeforeBind.Store(nil)
			if ps != nil {
				t.Cleanup(func() { closePubSub(t, ps) })
			}
			if ps != nil || !errors.Is(err, ErrQueueFull) {
				t.Fatalf("%s whose first control call failed with ErrQueueFull returned (%v, %v), want (nil, ErrQueueFull)", tc.name, ps, err)
			}
			c := built.Load()
			if c == nil {
				t.Fatal("fixture: the before-bind hook did not see the conn")
			}
			waitFor(t, "the constructor to release the conn of the PubSub it closed", 5*time.Second, c.closed.Load)
		})
	}
}

// (item 15) The reconnect loop's own check of the freshly installed conn
// (after the replay) is what makes a conn that dies during the replay count
// as a failed attempt, so the backoff keeps growing. Without it the attempt
// is reported ok, the backoff is reset, and the deferred recheck starts a new
// loop at attempt 0: a server that accepts and hangs up is retried at the
// base delay for ever. The test holds the replay's write after it returned,
// kills the conn, and measures the delay to the next attempt.
func TestReconnectLoopCountsAConnThatDiesDuringTheReplayAsFailed887(t *testing.T) {
	eachRigEngine(t, testReconnectLoopCountsAConnThatDiesDuringTheReplayAsFailed887)
}

func testReconnectLoopCountsAConnThatDiesDuringTheReplayAsFailed887(t *testing.T, eng rigEngine) {
	r := newPubsubRig(t, eng)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ps, err := r.cl.newPubSub(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closePubSub(t, ps) })
	ps.backoff = &async.Backoff{Base: 100 * time.Millisecond, Cap: 5 * time.Second, Jitter: 0}
	if err := ps.Subscribe(ctx, "news"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the server to see SUBSCRIBE", 5*time.Second, func() bool { return r.log.count("SUBSCRIBE") == 1 })

	r.gate.stampWrites(hasCmd("SUBSCRIBE"))
	r.gate.armAfter(hasCmd("SUBSCRIBE"))
	r.dropServerSide()
	select {
	case <-r.gate.afterHit: // attempt 0: the replay is written, the loop has not looked at the conn yet
	case <-time.After(10 * time.Second):
		t.Fatal("the reconnect never replayed the subscription")
	}
	nw := ps.connNow()
	if nw == nil || nw.closed.Load() {
		t.Fatal("fixture: the new conn is missing or already closed")
	}
	r.dropServerSide() // the new conn dies during the replay
	waitFor(t, "the new conn to be torn down (onClose)", 5*time.Second, nw.closed.Load)
	released := time.Now()
	close(r.gate.afterRelease)

	waitFor(t, "the next attempt to replay", 10*time.Second, func() bool { return len(r.gate.stampTimes()) >= 2 })
	// stamps[0] is the held replay (before the release); the next entry is the
	// following attempt's write.
	gap := r.gate.stampTimes()[1].Sub(released)
	t.Logf("887.15: next attempt's SUBSCRIBE %v after the release (backoff base 100 ms, attempt 1 = 200 ms, attempt 0 = 100 ms)", gap)
	if gap < 150*time.Millisecond {
		t.Errorf("the attempt after a conn that died during the replay came %v later, want >= 150 ms: the failed attempt did not grow the backoff", gap)
	}
}
