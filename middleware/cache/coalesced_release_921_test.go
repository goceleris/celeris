package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/middleware/internal/sf"
	"github.com/goceleris/celeris/middleware/store"
)

// celeris#921: the coalesced fill ran its producer (the handler, then the
// store's Set) with nothing to release the key if it panicked. The key's call
// stayed in the group's map and its followers' WaitGroup was never released:
// every follower already waiting, and every later request for the key, waited
// forever. On epoll, io_uring and adaptive each such request also held its
// event loop. And a Set that hung (a remote store) held every follower.

// hookStore wraps a store.KV; set, when not nil, runs at the start of each
// Set.
type hookStore struct {
	store.KV
	set func(n int32)
	n   atomic.Int32
}

func (s *hookStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	n := s.n.Add(1)
	if s.set != nil {
		s.set(n)
	}
	return s.KV.Set(ctx, key, value, ttl)
}

// answer921 is one request's outcome.
type answer921 struct {
	status   int
	body     string
	xcache   string
	err      error
	panicked any
	done     bool
}

// serve921 runs one GET /k through chain in a goroutine and returns a channel
// that delivers its answer. A panic in the chain is recovered and reported.
func serve921(t *testing.T, chain ...celeris.HandlerFunc) <-chan answer921 {
	t.Helper()
	out := make(chan answer921, 1)
	go func() {
		c, rec := celeristest.NewContext("GET", "/k", celeristest.WithHandlers(chain...))
		var a answer921
		func() {
			defer func() { a.panicked = recover() }()
			a.err = c.Next()
		}()
		a.status, a.body, a.xcache, a.done = rec.StatusCode, string(rec.Body), rec.Header("x-cache"), true
		celeristest.ReleaseContext(c)
		out <- a
	}()
	return out
}

func await921(t *testing.T, who string, ch <-chan answer921, within time.Duration) answer921 {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(within):
		t.Errorf("%s: no answer within %v (the key's coalesced call was never released)", who, within)
		return answer921{}
	}
}

// joinHook installs sf.TestHookFollowerJoined for the test and returns a
// channel that receives one value per follower that joins.
func joinHook(t *testing.T) <-chan struct{} {
	t.Helper()
	joined := make(chan struct{}, 64)
	prev := sf.TestHookFollowerJoined
	sf.TestHookFollowerJoined = func() { joined <- struct{}{} }
	t.Cleanup(func() { sf.TestHookFollowerJoined = prev })
	return joined
}

func waitJoined(t *testing.T, joined <-chan struct{}, n int) {
	t.Helper()
	for i := range n {
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of %d followers joined the leader", i, n)
		}
	}
}

// TestCachePanickingFillReleasesItsKey921: the fill's handler, or the store's
// Set, panics once. The panic stays the leader's: it reaches the leader's own
// request unchanged. A follower that waited on the panicking leader, and every
// later request for the key, must be answered: the follower runs its own
// handler, as it does when the leader's response is not cacheable.
func TestCachePanickingFillReleasesItsKey921(t *testing.T) {
	for _, where := range []string{"handler", "store-set"} {
		t.Run(where, func(t *testing.T) {
			joined := joinHook(t)
			entered, gate := make(chan struct{}), make(chan struct{})
			var runs atomic.Int32
			h := func(c *celeris.Context) error {
				n := runs.Add(1)
				if n == 1 {
					close(entered)
					<-gate
					if where == "handler" {
						panic("fill panicked")
					}
				}
				return c.String(200, "value")
			}
			kv := &hookStore{KV: store.NewMemoryKV()}
			if where == "store-set" {
				kv.set = func(n int32) {
					if n == 1 {
						panic("fill panicked")
					}
				}
			}
			mw := New(Config{Store: kv})

			leader := serve921(t, mw, h)
			<-entered
			follower := serve921(t, mw, h)
			waitJoined(t, joined, 1)
			close(gate)

			l := await921(t, "leader", leader, 5*time.Second)
			if l.done && l.panicked != "fill panicked" {
				t.Errorf("leader: panicked with %v (err %v), want its own panic \"fill panicked\"", l.panicked, l.err)
			}
			f := await921(t, "follower that waited on the panicking leader", follower, 3*time.Second)
			if f.done && (f.panicked != nil || f.status != 200 || f.body != "value") {
				t.Errorf("follower: %d %q panicked=%v err=%v, want 200 \"value\" from its own handler run", f.status, f.body, f.panicked, f.err)
			}
			// The key is free again. After a handler panic the follower ran
			// its own handler and stored the response, so a later request is
			// a HIT from the store. After a Set panic nothing was stored: a
			// later request leads a call of its own (MISS), where a call left
			// in place would hand it the panicked call's response.
			wantLater := "HIT"
			if where == "store-set" {
				wantLater = "MISS"
			}
			later := await921(t, "a later request for the key", serve921(t, mw, h), 3*time.Second)
			if later.done && (later.panicked != nil || later.status != 200 || later.body != "value" || later.xcache != wantLater) {
				t.Errorf("later request: %d %q x-cache %q panicked=%v err=%v, want 200 \"value\" %s", later.status, later.body, later.xcache, later.panicked, later.err, wantLater)
			}
			if again := await921(t, "the request after it", serve921(t, mw, h), 3*time.Second); again.done && (again.status != 200 || again.xcache != "HIT") {
				t.Errorf("the request after it: %d x-cache %q, want 200 HIT", again.status, again.xcache)
			}
			t.Logf("%s: leader panicked=%v; follower %d %q x-cache %q; later %d %q x-cache %q; handler runs %d",
				where, l.panicked, f.status, f.body, f.xcache, later.status, later.body, later.xcache, runs.Load())
		})
	}
}

// TestCacheHungSetDoesNotHoldFollowers921: the store's Set hangs (a remote
// store that does not answer). A follower that joined while the leader's
// handler ran, and one that arrives while the leader's Set hangs, must both
// get the leader's response without waiting for the Set: the coalesced call
// is over once the handler has run and its response is encoded. Only the
// leader's own request waits for its Set.
func TestCacheHungSetDoesNotHoldFollowers921(t *testing.T) {
	joined := joinHook(t)
	entered, gate := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	h := func(c *celeris.Context) error {
		if runs.Add(1) == 1 {
			close(entered)
			<-gate
		}
		return c.String(200, "value")
	}
	inSet, releaseSet := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseSet) }) })
	kv := &hookStore{KV: store.NewMemoryKV(), set: func(n int32) {
		if n == 1 {
			close(inSet)
			<-releaseSet
		}
	}}
	mw := New(Config{Store: kv})

	leader := serve921(t, mw, h)
	<-entered
	during := serve921(t, mw, h)
	waitJoined(t, joined, 1)
	close(gate)
	select {
	case <-inSet:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader's Set never started")
	}
	after := serve921(t, mw, h)

	for _, w := range []struct {
		name string
		ch   <-chan answer921
	}{{"follower that joined while the handler ran", during}, {"request that arrived while the Set hangs", after}} {
		a := await921(t, w.name, w.ch, 3*time.Second)
		if a.done && (a.status != 200 || a.body != "value" || a.panicked != nil || a.err != nil) {
			t.Errorf("%s: %d %q err=%v panicked=%v, want 200 \"value\"", w.name, a.status, a.body, a.err, a.panicked)
		}
		t.Logf("%s: %d %q x-cache %q done=%v", w.name, a.status, a.body, a.xcache, a.done)
	}
	if r := runs.Load(); r != 1 {
		t.Errorf("the handler ran %d times, want 1 (the followers take the leader's response)", r)
	}
	releaseOnce.Do(func() { close(releaseSet) })
	l := await921(t, "leader", leader, 5*time.Second)
	if l.done && (l.status != 200 || l.body != "value" || l.xcache != "MISS") {
		t.Errorf("leader: %d %q x-cache %q, want 200 \"value\" MISS", l.status, l.body, l.xcache)
	}
}

// TestCacheFollowerWaitEndsWithItsContext921: a follower waits for the leader
// only as long as its own request context lives (a timeout middleware, or a
// client that went away), as middleware/singleflight's waiters do.
func TestCacheFollowerWaitEndsWithItsContext921(t *testing.T) {
	joined := joinHook(t)
	entered, gate := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(gate) })
	h := func(c *celeris.Context) error {
		select {
		case <-entered:
		default:
			close(entered)
			<-gate
		}
		return c.String(200, "value")
	}
	mw := New(Config{})
	leader := serve921(t, mw, h)
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	withCtx := func(c *celeris.Context) error {
		c.SetContext(ctx)
		return c.Next()
	}
	follower := serve921(t, withCtx, mw, h)
	waitJoined(t, joined, 1)
	cancel()
	f := await921(t, "follower whose context ended", follower, 3*time.Second)
	if f.done && !errors.Is(f.err, context.Canceled) {
		t.Errorf("follower: err %v status %d, want context.Canceled", f.err, f.status)
	}
	_ = leader
}

// TestCacheConfigCoalescesByDefault922: celeris#922. Singleflight was
// documented as on by default, but a Config literal that did not set it got
// false, so New(Config{...}) did not coalesce: ten concurrent misses ran the
// handler ten times.
func TestCacheConfigCoalescesByDefault922(t *testing.T) {
	for _, tc := range []struct {
		name string
		mw   func() celeris.HandlerFunc
	}{
		{"New()", func() celeris.HandlerFunc { return New() }},
		{"New(Config{TTL})", func() celeris.HandlerFunc { return New(Config{TTL: time.Minute}) }},
		{"New(Config{VaryHeaders})", func() celeris.HandlerFunc { return New(Config{VaryHeaders: []string{"Accept-Encoding"}}) }},
		{"New(Config{Store})", func() celeris.HandlerFunc { return New(Config{Store: store.NewMemoryKV()}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			joined := joinHook(t)
			const n = 10
			entered, gate := make(chan struct{}), make(chan struct{})
			var runs atomic.Int32
			h := func(c *celeris.Context) error {
				if runs.Add(1) == 1 {
					close(entered)
					<-gate
				}
				return c.String(200, "value")
			}
			mw := tc.mw()
			answers := []<-chan answer921{serve921(t, mw, h)}
			<-entered
			for range n - 1 {
				answers = append(answers, serve921(t, mw, h))
			}
			// The followers join the leader; without coalescing they run
			// the handler themselves and never join.
			deadline := time.After(2 * time.Second)
			got := 0
		wait:
			for got < n-1 {
				select {
				case <-joined:
					got++
				case <-deadline:
					break wait
				}
			}
			close(gate)
			for i, ch := range answers {
				a := await921(t, "request", ch, 5*time.Second)
				if a.done && (a.status != 200 || a.body != "value") {
					t.Errorf("request %d: %d %q", i, a.status, a.body)
				}
			}
			if r := runs.Load(); r != 1 {
				t.Errorf("%d concurrent misses ran the handler %d times (%d joined the leader), want 1", n, r, got)
			}
			t.Logf("%s: %d concurrent misses, %d joined, handler runs %d", tc.name, n, got, runs.Load())
		})
	}
}

// TestCacheIgnoreCacheControl922: celeris#922's other half.
// RespectCacheControl: false had no effect (applyDefaults set it back to
// true), so no Config could store a no-store response. IgnoreCacheControl
// replaces it: by default a no-store response is not stored, and with
// IgnoreCacheControl it is.
func TestCacheIgnoreCacheControl922(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      Config
		wantRuns int32
		second   string
	}{
		{"default", Config{TTL: time.Minute}, 2, "MISS"},
		{"IgnoreCacheControl", Config{TTL: time.Minute, IgnoreCacheControl: true}, 1, "HIT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var runs atomic.Int32
			h := func(c *celeris.Context) error {
				runs.Add(1)
				c.SetHeader("cache-control", "no-store")
				return c.String(200, "value")
			}
			mw := New(tc.cfg)
			first := await921(t, "first", serve921(t, mw, h), 3*time.Second)
			second := await921(t, "second", serve921(t, mw, h), 3*time.Second)
			if first.xcache != "MISS" || second.xcache != tc.second || second.body != "value" {
				t.Errorf("x-cache %q then %q (%q), want MISS then %s", first.xcache, second.xcache, second.body, tc.second)
			}
			if r := runs.Load(); r != tc.wantRuns {
				t.Errorf("handler ran %d times, want %d", r, tc.wantRuns)
			}
		})
	}
}
