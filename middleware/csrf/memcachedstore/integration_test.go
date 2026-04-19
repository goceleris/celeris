//go:build integration

package memcachedstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/middleware/store"
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

func TestCSRFMemcachedLive_GetAndDelete(t *testing.T) {
	c := newLiveClient(t)
	prefix := fmt.Sprintf("csrf-live-%d:", time.Now().UnixNano())
	s := New(c, Options{KeyPrefix: prefix})
	ctx := context.Background()

	_ = s.Set(ctx, "tok", []byte("abc"), time.Minute)
	v, err := s.GetAndDelete(ctx, "tok")
	if err != nil || string(v) != "abc" {
		t.Fatalf("GetAndDelete: got=%q err=%v", v, err)
	}
	if _, err := s.GetAndDelete(ctx, "tok"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second: expected ErrNotFound, got %v", err)
	}
}

func TestCSRFMemcachedLive_CASProtectsSingleUse(t *testing.T) {
	c := newLiveClient(t)
	prefix := fmt.Sprintf("csrf-live-%d:", time.Now().UnixNano())
	s := New(c, Options{KeyPrefix: prefix})
	ctx := context.Background()
	_ = s.Set(ctx, "race", []byte("tok"), time.Minute)

	var winners atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 4; i++ {
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
		t.Fatalf("live CAS: expected exactly 1 winner, got %d", got)
	}
}
