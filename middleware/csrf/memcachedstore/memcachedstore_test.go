package memcachedstore

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/middleware/internal/fakememcached"
	"github.com/goceleris/celeris/middleware/store"
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

func TestSetGetDelete(t *testing.T) {
	c := newTestClient(t)
	s := New(c)
	ctx := context.Background()

	if err := s.Set(ctx, "t1", []byte("abc"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, "t1")
	if err != nil || string(got) != "abc" {
		t.Fatalf("Get: got=%q err=%v", got, err)
	}
	if err := s.Delete(ctx, "t1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "t1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after Delete: %v", err)
	}
}

func TestGetAndDeleteReturnsValueAndClears(t *testing.T) {
	c := newTestClient(t)
	s := New(c)
	ctx := context.Background()
	_ = s.Set(ctx, "tok", []byte("xyz"), time.Minute)

	v, err := s.GetAndDelete(ctx, "tok")
	if err != nil || string(v) != "xyz" {
		t.Fatalf("GetAndDelete: got=%q err=%v", v, err)
	}
	// Second call misses.
	if _, err := s.GetAndDelete(ctx, "tok"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second GetAndDelete: expected ErrNotFound, got %v", err)
	}
}

func TestGetAndDeleteIsCASProtected(t *testing.T) {
	// Two goroutines race to consume the same single-use token.
	// Exactly one wins; the other observes ErrNotFound.
	c := newTestClient(t)
	s := New(c)
	ctx := context.Background()
	_ = s.Set(ctx, "race", []byte("tok"), time.Minute)

	var winners atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			v, err := s.GetAndDelete(ctx, "race")
			if err == nil && string(v) == "tok" {
				winners.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("expected exactly 1 winner, got %d", got)
	}
}

func TestGetAndDeleteMissingIsErrNotFound(t *testing.T) {
	c := newTestClient(t)
	s := New(c)
	if _, err := s.GetAndDelete(context.Background(), "absent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestInterfaceConformance(t *testing.T) {
	c := newTestClient(t)
	var _ store.KV = New(c)
	var _ store.GetAndDeleter = New(c)
}
