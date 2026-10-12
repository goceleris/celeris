package cache

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#888 item 30 (from #940 review r2): a fill's Set must not overwrite
// a fresher entry written meanwhile. Two fills of one key can overlap: with
// DisableSingleflight every concurrent miss runs the handler, and with
// singleflight a call that was replaced after its result expired may still
// have a Set in flight (a Set that hangs and then returns). The fill that
// started later read the origin later, so its entry is the fresher one, and
// a Set from the earlier fill that lands after it must not replace it.

// versioned888 is the ordered write the cache's own MemoryStore offers (the
// production side declares it as versionedSetter): the test store forwards it,
// or the test would only exercise the plain Set.
type versioned888 interface {
	setIfNewer(ctx context.Context, key string, value []byte, ttl time.Duration, gen uint64) error
}

// gateStore wraps a MemoryStore; before, when not nil, runs at the start of
// each write (Set, or the ordered write), with the write's number.
type gateStore struct {
	*MemoryStore
	before func(n int32)
	n      atomic.Int32
}

func (s *gateStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if n := s.n.Add(1); s.before != nil {
		s.before(n)
	}
	return s.MemoryStore.Set(ctx, key, value, ttl)
}

func (s *gateStore) setIfNewer(ctx context.Context, key string, value []byte, ttl time.Duration, gen uint64) error {
	if n := s.n.Add(1); s.before != nil {
		s.before(n)
	}
	if vs, ok := any(s.MemoryStore).(versioned888); ok {
		return vs.setIfNewer(ctx, key, value, ttl, gen)
	}
	return s.MemoryStore.Set(ctx, key, value, ttl)
}

// TestCacheStaleFillDoesNotOverwriteFresherEntry888: without singleflight, a
// request that missed first but whose handler finished last must not replace
// the entry of the request that missed later and finished first.
func TestCacheStaleFillDoesNotOverwriteFresherEntry888(t *testing.T) {
	kv := &gateStore{MemoryStore: NewMemoryStore()}
	t.Cleanup(kv.Close)
	entered, gate := make(chan struct{}), make(chan struct{})
	var runs atomic.Int32
	h := func(c *celeris.Context) error {
		n := runs.Add(1)
		if n == 1 {
			close(entered)
			<-gate // a slow backend call: this run reads the origin early and answers late
		}
		return c.String(200, "v%d", n)
	}
	mw := New(Config{Store: kv, DisableSingleflight: true})

	slow := serve921(t, mw, h)
	<-entered
	if fast := await921(t, "the later request", serve921(t, mw, h), 3*time.Second); fast.body != "v2" || fast.xcache != "MISS" {
		t.Fatalf("the later request: %q x-cache %q, want v2 MISS", fast.body, fast.xcache)
	}
	close(gate)
	if a := await921(t, "the earlier request", slow, 3*time.Second); a.body != "v1" || a.xcache != "MISS" {
		t.Fatalf("the earlier request: %q x-cache %q, want its own v1 MISS", a.body, a.xcache)
	}
	// The earlier fill stored after the later one. The entry must still be
	// the later (fresher) fill's.
	if third := await921(t, "a request after both", serve921(t, mw, h), 3*time.Second); third.body != "v2" || third.xcache != "HIT" {
		t.Errorf("a request after both fills: %q x-cache %q, want v2 HIT (the stale fill's Set replaced the fresher entry)", third.body, third.xcache)
	}
}

// TestCacheReplacedCallsHungSetDoesNotOverwriteTheNewerEntry888: with
// singleflight. The leader's Set hangs; its response expires; a request
// leads a call of its own (the replaced call keeps its Set), stores a fresher
// response; then the hung Set returns. The newer entry must stay.
func TestCacheReplacedCallsHungSetDoesNotOverwriteTheNewerEntry888(t *testing.T) {
	inSet, releaseSet := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(releaseSet) }) })
	kv := &gateStore{MemoryStore: NewMemoryStore(), before: func(n int32) {
		if n == 1 {
			close(inSet)
			<-releaseSet
		}
	}}
	t.Cleanup(kv.Close)
	var runs atomic.Int32
	h := func(c *celeris.Context) error {
		n := runs.Add(1)
		if n == 1 {
			c.SetHeader("cache-control", "max-age=1") // fresh for 1 s, then replaceable
		} else {
			c.SetHeader("cache-control", "max-age=60")
		}
		return c.String(200, "v%d", n)
	}
	mw := New(Config{Store: kv, TTL: time.Minute})

	first := serve921(t, mw, h)
	select {
	case <-inSet:
	case <-time.After(5 * time.Second):
		t.Fatal("the leader's Set never started")
	}
	time.Sleep(1100 * time.Millisecond) // the held result is past its freshness
	if second := await921(t, "the request that leads a new call", serve921(t, mw, h), 3*time.Second); second.body != "v2" || second.xcache != "MISS" {
		t.Fatalf("the request after the freshness: %q x-cache %q, want v2 MISS from its own handler run", second.body, second.xcache)
	}
	once.Do(func() { close(releaseSet) }) // the hung Set returns, after the newer Set
	if a := await921(t, "the first leader", first, 3*time.Second); a.body != "v1" {
		t.Fatalf("the first leader: %q, want its own v1", a.body)
	}
	if third := await921(t, "a request after both", serve921(t, mw, h), 3*time.Second); third.body != "v2" || third.xcache != "HIT" {
		t.Errorf("a request after both: %q x-cache %q, want v2 HIT (the replaced call's Set replaced the newer entry)", third.body, third.xcache)
	}
}
