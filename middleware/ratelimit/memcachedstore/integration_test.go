//go:build integration

package memcachedstore

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
)

func newLiveClient(t *testing.T) *celmc.Client {
	t.Helper()
	addr := os.Getenv("CELERIS_MEMCACHED_ADDR")
	if addr == "" {
		t.Skip("CELERIS_MEMCACHED_ADDR unset")
	}
	c, err := celmc.NewClient(addr)
	if err != nil {
		t.Fatalf("celmc.NewClient(%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRatelimitMemcachedLive_TokenBucket(t *testing.T) {
	c := newLiveClient(t)
	prefix := fmt.Sprintf("rl-live-%d:", time.Now().UnixNano())
	s, err := New(c, Options{KeyPrefix: prefix, RPS: 1, Burst: 5})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 5; i++ {
		ok, _, _, err := s.Allow("user")
		if err != nil || !ok {
			t.Fatalf("Allow %d: ok=%v err=%v", i, ok, err)
		}
	}
	ok, _, _, _ := s.Allow("user")
	if ok {
		t.Fatal("Allow 6: expected rate-limited")
	}
}

func TestRatelimitMemcachedLive_ConcurrentRespectsBurst(t *testing.T) {
	c := newLiveClient(t)
	prefix := fmt.Sprintf("rl-live-%d:", time.Now().UnixNano())
	s, err := New(c, Options{KeyPrefix: prefix, RPS: 1, Burst: 3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var allowed atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if ok, _, _, _ := s.Allow("live"); ok {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	got := allowed.Load()
	// Safety invariant: CAS-based rate limiting may over-throttle
	// under contention but MUST NEVER exceed the burst.
	if got > 3 {
		t.Fatalf("live concurrent: %d > burst 3 (safety violation) retries=%d", got, s.RetriesTotal())
	}
	if got == 0 {
		t.Fatalf("live concurrent: 0 winners (starvation) retries=%d", s.RetriesTotal())
	}
	t.Logf("winners=%d/10 (retries=%d)", got, s.RetriesTotal())
}
