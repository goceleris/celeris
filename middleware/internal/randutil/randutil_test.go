package randutil

import (
	"sync"
	"testing"
)

func TestHexTokenLength(t *testing.T) {
	for _, n := range []int{1, 16, 32, 64, 128} {
		tok := HexToken(n)
		if len(tok) != 2*n {
			t.Fatalf("HexToken(%d): expected length %d, got %d", n, 2*n, len(tok))
		}
	}
}

func TestHexTokenUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for range 1000 {
		tok := HexToken(32)
		if _, ok := seen[tok]; ok {
			t.Fatal("generated duplicate token")
		}
		seen[tok] = struct{}{}
	}
}

func TestHexTokenConcurrency(t *testing.T) {
	const goroutines = 10
	const tokensPerGoroutine = 100

	var mu sync.Mutex
	seen := make(map[string]struct{}, goroutines*tokensPerGoroutine)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()
			local := make([]string, 0, tokensPerGoroutine)
			for range tokensPerGoroutine {
				local = append(local, HexToken(32))
			}
			mu.Lock()
			for _, tok := range local {
				if _, ok := seen[tok]; ok {
					t.Error("generated duplicate token under concurrency")
				}
				seen[tok] = struct{}{}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
}
