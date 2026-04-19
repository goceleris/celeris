package memcachedstore

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/middleware/internal/fakememcached"
)

func newTestClient(t *testing.T) *celmc.Client {
	t.Helper()
	srv := fakememcached.Start(t)
	c, err := celmc.NewClient(srv.Addr())
	if err != nil {
		t.Fatalf("celmc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestAllowUnderLimit(t *testing.T) {
	c := newTestClient(t)
	s, err := New(c, Options{RPS: 1, Burst: 5})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 5; i++ {
		ok, _, _, err := s.Allow("user:a")
		if err != nil {
			t.Fatalf("Allow %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("Allow %d: expected true", i)
		}
	}
	ok, _, _, err := s.Allow("user:a")
	if err != nil {
		t.Fatalf("Allow 6: %v", err)
	}
	if ok {
		t.Fatal("Allow 6: expected rate-limited")
	}
}

func TestAllowRefill(t *testing.T) {
	c := newTestClient(t)
	s, err := New(c, Options{RPS: 1000, Burst: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ok, _, _, _ := s.Allow("u")
	if !ok {
		t.Fatal("first Allow: expected true")
	}
	ok, _, _, _ = s.Allow("u")
	if ok {
		t.Fatal("immediate second Allow: expected false")
	}
	// After 5ms at 1000 RPS → ~5 tokens, one allow succeeds.
	time.Sleep(5 * time.Millisecond)
	ok, _, _, _ = s.Allow("u")
	if !ok {
		t.Fatal("Allow after refill: expected true")
	}
}

func TestUndoReturnsToken(t *testing.T) {
	c := newTestClient(t)
	s, err := New(c, Options{RPS: 1, Burst: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ok, _, _, _ := s.Allow("k")
	if !ok {
		t.Fatal("first Allow failed")
	}
	if err := s.Undo("k"); err != nil {
		t.Fatalf("Undo: %v", err)
	}
	ok, _, _, _ = s.Allow("k")
	if !ok {
		t.Fatal("after Undo, Allow expected true")
	}
}

func TestPerKeyIsolation(t *testing.T) {
	c := newTestClient(t)
	s, err := New(c, Options{RPS: 1, Burst: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	okA, _, _, _ := s.Allow("a")
	okB, _, _, _ := s.Allow("b")
	if !okA || !okB {
		t.Fatal("independent keys should each get their burst")
	}
	okA, _, _, _ = s.Allow("a")
	if okA {
		t.Fatal("second Allow on same key should deny")
	}
}

func TestConcurrentAllowRespectsBurst(t *testing.T) {
	// 20 goroutines race for a burst of 5. Exactly 5 succeed; the
	// remaining 15 are denied. Validates the CAS loop correctness.
	c := newTestClient(t)
	s, err := New(c, Options{RPS: 1, Burst: 5})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var allowed atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, _, _, err := s.Allow("concurrent")
			if err == nil && ok {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := allowed.Load(); got != 5 {
		t.Fatalf("concurrent Allow: got %d winners, want 5", got)
	}
	if s.RetriesTotal() == 0 {
		t.Log("note: CAS loop had 0 retries — contention may not have been exercised")
	}
}
