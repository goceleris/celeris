//go:build redis

package redis_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

// A released Pipeline is gone: every later call on the stale pointer must
// fail cleanly and must never reach a Pipeline another caller holds. The
// tests loop because the pooled buffers are handed to the next Client.Pipeline
// call only some of the time (sync.Pool drops a random share of Puts under
// -race).

func TestPipeline_StaleReleaseAndDiscard_DoNotTouchAnotherCallersPipeline(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 20*time.Second)
	for i := 0; i < 200; i++ {
		key := uniqueKey(t, fmt.Sprintf("k%d", i))
		cleanupKeys(t, c, key)
		p1 := c.Pipeline()
		p1.Release()
		p2 := c.Pipeline() // another caller: usually receives p1's buffers
		set := p2.Set(key, "mine", 0)
		p1.Release() // a late deferred Release on the stale pointer
		p1.Discard() // and a late Discard
		if err := p2.Exec(ctx); err != nil {
			t.Fatalf("iteration %d: the other caller's Exec: %v", i, err)
		}
		if _, err := set.Result(); err != nil {
			t.Fatalf("iteration %d: the other caller's SET: %v", i, err)
		}
		if v, err := c.Get(ctx, key); err != nil || v != "mine" {
			t.Fatalf("iteration %d: the other caller's write was lost: GET = %q, %v", i, v, err)
		}
		p2.Release()
	}
}

func TestPipeline_EnqueueAndExecAfterRelease_ReturnErrors(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 5*time.Second)
	key := uniqueKey(t, "k")
	cleanupKeys(t, c, key)
	p := c.Pipeline()
	p.Release()

	var (
		s *celredis.StatusCmd
		g *celredis.StringCmd
		i *celredis.IntCmd
		b *celredis.BoolCmd
	)
	if pv := panicOf(func() {
		s = p.Set(key, "v", 0)
		g = p.Get(key)
		i = p.Incr(key + ":n")
		b = p.Expire(key, time.Minute)
		_ = p.Del(key, key+":n")
		_ = p.HSet(key+":h", "f", "v")
	}); pv != nil {
		t.Fatalf("queueing on a released Pipeline panicked: %v", pv)
	}
	var err error
	if pv := panicOf(func() { err = p.Exec(ctx) }); pv != nil {
		t.Fatalf("Exec on a released Pipeline panicked: %v", pv)
	}
	if err == nil {
		t.Fatal("Exec on a released Pipeline returned nil")
	}
	resultErrors(t, "after Release", map[string]func() error{
		"StatusCmd": func() error { _, e := s.Result(); return e },
		"StringCmd": func() error { _, e := g.Result(); return e },
		"IntCmd":    func() error { _, e := i.Result(); return e },
		"BoolCmd":   func() error { _, e := b.Result(); return e },
	})
	if _, err := c.Get(ctx, key); !errors.Is(err, celredis.ErrNil) {
		t.Fatalf("a command queued after Release was sent: GET = %v", err)
	}
}

func TestPipeline_StaleCmdHandle_DoesNotReadAnotherCallersValue(t *testing.T) {
	c := connectClient(t)
	ctx := ctxWithTimeout(t, 20*time.Second)
	ka, kb := uniqueKey(t, "a"), uniqueKey(t, "b")
	cleanupKeys(t, c, ka, kb)
	if err := c.Set(ctx, ka, "AAAA", 0); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(ctx, kb, "BBBB", 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		p1 := c.Pipeline()
		g1 := p1.Get(ka)
		if err := p1.Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if v, err := g1.Result(); err != nil || v != "AAAA" {
			t.Fatalf("g1 before Release = %q, %v", v, err)
		}
		p1.Release()
		p2 := c.Pipeline()
		g2 := p2.Get(kb)
		if err := p2.Exec(ctx); err != nil {
			t.Fatal(err)
		}
		if v, err := g2.Result(); err != nil || v != "BBBB" {
			t.Fatalf("g2 = %q, %v", v, err)
		}
		// g1 is stale: it must report an error, never the other caller's value.
		if v, err := g1.Result(); err == nil || v == "BBBB" {
			t.Fatalf("iteration %d: stale handle g1.Result() = %q, %v", i, v, err)
		}
		p2.Release()
	}
}

// TestPipeline_StaleHandleMisuse_NeverReachesOtherCallers runs workers that
// pipeline with their own marker values while one goroutine keeps misusing
// pipelines it already released. Every worker checks every marker it reads
// and that its writes land.
func TestPipeline_StaleHandleMisuse_NeverReachesOtherCallers(t *testing.T) {
	c := connectClient(t)
	const workers = 8
	dur := 3 * time.Second
	if testing.Short() {
		dur = 500 * time.Millisecond
	}
	ctxAll := ctxWithTimeout(t, dur+30*time.Second)
	base := uniqueKey(t, "w")
	keys := make([]string, workers)
	for w := range keys {
		keys[w] = fmt.Sprintf("%s:%d", base, w)
	}
	staleKey := base + ":stale"
	cleanupKeys(t, c, append(keys, staleKey)...)

	var stop atomic.Bool
	var wg sync.WaitGroup
	var failures atomic.Int64
	fail := func(format string, a ...any) {
		if failures.Add(1) <= 10 {
			t.Errorf(format, a...)
		}
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for n := 0; !stop.Load(); n++ {
				marker := fmt.Sprintf("worker-%d-round-%d", w, n)
				p := c.Pipeline()
				set := p.Set(keys[w], marker, 0)
				get := p.Get(keys[w])
				if err := p.Exec(ctxAll); err != nil {
					fail("worker %d: Exec: %v", w, err)
					p.Release()
					return
				}
				if _, err := set.Result(); err != nil {
					fail("worker %d: SET: %v", w, err)
				}
				if v, err := get.Result(); err != nil || v != marker {
					fail("worker %d round %d: GET = %q, %v; want %q", w, n, v, err, marker)
				}
				p.Release()
				if v, err := c.Get(ctxAll, keys[w]); err != nil || v != marker {
					fail("worker %d round %d: the write did not land: GET = %q, %v; want %q", w, n, v, err, marker)
				}
			}
		}(w)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		stale := make([]*celredis.Pipeline, 0, 4)
		var handles []*celredis.StringCmd
		for !stop.Load() {
			p := c.Pipeline()
			handles = append(handles, p.Get(keys[0]))
			p.Release()
			stale = append(stale, p)
			if len(stale) == cap(stale) {
				for _, sp := range stale {
					for _, op := range []func(){
						sp.Release,
						sp.Discard,
						func() { sp.Set(staleKey, "stale-write", 0) },
						func() { sp.Get(keys[2]) },
						func() { _ = sp.Exec(ctxAll) },
					} {
						if pv := panicOf(op); pv != nil {
							fail("a stale Pipeline call panicked: %v", pv)
						}
					}
				}
				for _, h := range handles {
					if pv := panicOf(func() { _, _ = h.Result() }); pv != nil {
						fail("a stale handle's Result panicked: %v", pv)
					}
				}
				stale, handles = stale[:0], handles[:0]
			}
		}
	}()

	time.Sleep(dur)
	stop.Store(true)
	wg.Wait()
	if failures.Load() > 0 {
		t.Fatalf("%d failures", failures.Load())
	}
	if v, err := c.Get(ctxAll, staleKey); err == nil {
		t.Fatalf("a stale pipeline's write landed on %s: %q", staleKey, v)
	}
}
