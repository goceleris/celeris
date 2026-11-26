//go:build redis

package redis_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	celredis "github.com/goceleris/celeris/driver/redis"
)

// TestClient_Watch_NoUseAfterRelease drives many concurrent Watch calls: the
// WATCH reply's error used to be read after the request went back to its
// pool, where the next command overwrites it (a data race under -race, and a
// WATCH error that could be missed or belong to another caller).
func TestClient_Watch_NoUseAfterRelease(t *testing.T) {
	c := connectClient(t)
	key := uniqueKey(t, "w")
	keys := []string{key}
	for g := 0; g < 8; g++ {
		keys = append(keys, fmt.Sprintf("%s:%d", key, g))
	}
	cleanupKeys(t, c, keys...)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := c.Watch(ctx, func(tx *celredis.Tx) error { return nil }, key)
				if err == nil {
					_, err = c.Incr(ctx, fmt.Sprintf("%s:%d", key, g))
				}
				cancel()
				if err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Watch/Incr: %v", err)
	}
}
