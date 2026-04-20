package benchcmp_db

// Memory-leak hunt. Runs a celeris server with a realistic middleware
// stack + full v1.5.0 driver adapters under sustained load, captures
// heap profiles at 4 checkpoints (30s warmup, then 3× at LEAK_INTERVAL),
// and asserts that heap_in_use growth between the last two samples is
// within LEAK_TOLERANCE_PCT.
//
// Default config catches linear leaks within ~3 minutes; bump LEAK_TOTAL
// for overnight confidence. Opt-in via LEAK_HUNT=1.

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type heapSample struct {
	t          time.Time
	heapInUse  uint64
	heapAlloc  uint64
	goroutines int
}

func snapshotHeap() heapSample {
	runtime.GC()
	debug.FreeOSMemory()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return heapSample{
		t:          time.Now(),
		heapInUse:  m.HeapInuse,
		heapAlloc:  m.HeapAlloc,
		goroutines: runtime.NumGoroutine(),
	}
}

func TestLeakHuntCelerisRedis(t *testing.T) {
	if os.Getenv("LEAK_HUNT") != "1" {
		t.Skip("opt-in: set LEAK_HUNT=1 to run (long)")
	}
	// Short defaults suitable for CI smoke; bump for overnight runs.
	total := 3 * time.Minute
	interval := 30 * time.Second
	tolerancePct := 15.0
	concurrency := 32
	if v := os.Getenv("LEAK_TOTAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			total = d
		}
	}
	if v := os.Getenv("LEAK_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			interval = d
		}
	}
	if v := os.Getenv("LEAK_TOLERANCE_PCT"); v != "" {
		fmt.Sscanf(v, "%f", &tolerancePct)
	}
	if v := os.Getenv("LEAK_CONC"); v != "" {
		fmt.Sscanf(v, "%d", &concurrency)
	}

	redisAddr := skipIfNoRedis(t)
	if err := primeSession(redisAddr); err != nil {
		t.Skipf("prime: %v", err)
	}
	addr := startCelerisSessionServer(t, redisAddr)
	url := "http://" + addr + "/me"
	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: concurrency * 2,
			MaxConnsPerHost:     concurrency * 2,
			IdleConnTimeout:     60 * time.Second,
		},
		Timeout: 2 * time.Second,
	}

	var okCount, errCount atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				req, err := http.NewRequest("GET", url, nil)
				if err != nil {
					errCount.Add(1)
					continue
				}
				req.Header.Set(sessHeader, sessKey)
				resp, err := client.Do(req)
				if err != nil {
					errCount.Add(1)
					continue
				}
				_ = resp.Body.Close()
				if resp.StatusCode == 200 {
					okCount.Add(1)
				} else {
					errCount.Add(1)
				}
			}
		}()
	}

	// Let the heap settle (warmup).
	t.Log("leak hunt: warmup 30s...")
	time.Sleep(30 * time.Second)
	var samples []heapSample
	samples = append(samples, snapshotHeap())
	t.Logf("sample 0 (post-warmup): heap_in_use=%d KiB heap_alloc=%d KiB goroutines=%d",
		samples[0].heapInUse/1024, samples[0].heapAlloc/1024, samples[0].goroutines)

	start := time.Now()
	for i := 1; time.Since(start) < total; i++ {
		time.Sleep(interval)
		s := snapshotHeap()
		samples = append(samples, s)
		t.Logf("sample %d (+%v): heap_in_use=%d KiB heap_alloc=%d KiB goroutines=%d ok=%d err=%d",
			i, s.t.Sub(samples[0].t), s.heapInUse/1024, s.heapAlloc/1024, s.goroutines,
			okCount.Load(), errCount.Load())
	}

	close(stop)
	wg.Wait()

	// Dump final heap profile for post-hoc inspection.
	if path := os.Getenv("LEAK_HEAP_PROFILE"); path != "" {
		f, err := os.Create(path)
		if err == nil {
			_ = pprof.Lookup("heap").WriteTo(f, 0)
			_ = f.Close()
			t.Logf("wrote heap profile to %s", path)
		}
	}

	if len(samples) < 3 {
		t.Skip("not enough samples collected — run longer")
	}
	first := samples[0]
	last := samples[len(samples)-1]
	growth := int64(last.heapInUse) - int64(first.heapInUse)
	pct := float64(growth) / float64(first.heapInUse) * 100
	t.Logf("heap_in_use growth: %+d KiB (%.2f%%) over %v — tolerance %.2f%%",
		growth/1024, pct, last.t.Sub(first.t), tolerancePct)

	if pct > tolerancePct {
		t.Errorf("suspected memory leak: heap_in_use grew %.2f%% (tolerance %.2f%%)",
			pct, tolerancePct)
	}

	// Goroutine leak: count at end should be within ±10 of post-warmup.
	grDelta := last.goroutines - first.goroutines
	if grDelta > 10 {
		t.Errorf("suspected goroutine leak: %+d goroutines over test (%d → %d)",
			grDelta, first.goroutines, last.goroutines)
	}
}
