package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// TestShardedLimiterRace hammers Allow() from many goroutines while
// concurrently cancelling the CleanupContext that drives shard
// eviction. Run with `go test -race` to surface any races between
// the cleanup goroutine's map writes and Allow's reads/writes.
func TestShardedLimiterRace(t *testing.T) {
	cleanupCtx, cancel := context.WithCancel(context.Background())

	mw := New(Config{
		RPS:             1e9,
		Burst:           1 << 20,
		CleanupContext:  cleanupCtx,
		CleanupInterval: 5 * time.Millisecond, // tight loop forces eviction overlap
		DisableHeaders:  true,
	})

	const goroutines = 64
	const perGoroutine = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				ctx, _ := celeristest.NewContext("GET", "/api",
					celeristest.WithRemoteAddr(byteToIP(byte(id))+":1234"),
					celeristest.WithHandlers(mw, func(c *celeris.Context) error { _ = c; return nil }),
				)
				_ = ctx.Next()
				celeristest.ReleaseContext(ctx)
			}
		}(g)
	}

	// Cancel mid-flight to race cleanup termination with Allow.
	time.AfterFunc(20*time.Millisecond, cancel)
	wg.Wait()
}

func byteToIP(b byte) string {
	return "10.0.0." + itoa(int(b))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [3]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = '0' + byte(n%10)
		n /= 10
	}
	return string(buf[i:])
}
