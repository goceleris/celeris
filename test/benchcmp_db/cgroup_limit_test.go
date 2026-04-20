package benchcmp_db

// Simulates a cgroup-limited deployment (GOMEMLIMIT + GOMAXPROCS=1)
// and verifies the celeris + driver/redis stack stays within its
// constraints under sustained load without OOM or handler panics.
//
// True cgroup isolation (e.g. Docker/systemd-run) would confirm the
// same invariants with kernel enforcement. This in-process variant
// covers the hot-path behaviour the GC/runtime can actually see.
//
// Opt-in via CGROUP_SIM=1.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCgroupSimulatedLimitsHoldUnderLoad(t *testing.T) {
	if os.Getenv("CGROUP_SIM") != "1" {
		t.Skip("opt-in: set CGROUP_SIM=1 to run")
	}

	// Tight memory budget. 128MiB is far below typical default heap growth;
	// if the stack leaks, GC pressure will surface quickly.
	memLimit := int64(128 << 20)
	if v := os.Getenv("CGROUP_MEM"); v != "" {
		fmt.Sscanf(v, "%d", &memLimit)
	}
	// Restrict scheduling to 1 CPU to mimic cgroup cpus=1.
	prevMaxProcs := runtime.GOMAXPROCS(1)
	t.Cleanup(func() { runtime.GOMAXPROCS(prevMaxProcs) })
	prevLimit := debug.SetMemoryLimit(memLimit)
	t.Cleanup(func() { debug.SetMemoryLimit(prevLimit) })
	t.Logf("simulated cgroup: GOMAXPROCS=1 GOMEMLIMIT=%d MiB", memLimit/(1<<20))

	redisAddr := skipIfNoRedis(t)
	if err := primeSession(redisAddr); err != nil {
		t.Skipf("prime: %v", err)
	}
	addr := startCelerisSessionServer(t, redisAddr)
	url := "http://" + addr + "/me"

	// Drive load for CGROUP_DURATION.
	duration := 20 * time.Second
	if v := os.Getenv("CGROUP_DURATION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			duration = d
		}
	}
	concurrency := 32
	if v := os.Getenv("CGROUP_CONC"); v != "" {
		fmt.Sscanf(v, "%d", &concurrency)
	}

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: concurrency,
			MaxConnsPerHost:     concurrency,
			IdleConnTimeout:     30 * time.Second,
		},
		Timeout: 3 * time.Second,
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
				req, _ := http.NewRequest("GET", url, nil)
				req.Header.Set(sessHeader, sessKey)
				resp, err := client.Do(req)
				if err != nil {
					errCount.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode == 200 {
					okCount.Add(1)
				} else {
					errCount.Add(1)
				}
			}
		}()
	}

	time.Sleep(duration)
	close(stop)
	wg.Wait()

	ok := okCount.Load()
	errs := errCount.Load()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	t.Logf("under %d MiB GOMEMLIMIT / 1 CPU for %v: ok=%d err=%d heap_in_use=%d MiB num_gc=%d",
		memLimit/(1<<20), duration, ok, errs, m.HeapInuse/(1<<20), m.NumGC)

	// Assertions:
	//   - NonZeroThroughput: cgroup constraints shouldn't halt the server.
	//   - ErrorBudget: error rate stays below 1% (GC pressure MAY cause
	//     timeouts under extreme limits; 1% is generous).
	//   - HeapBelowLimit: reported HeapInuse stays under GOMEMLIMIT
	//     (the runtime enforces this via aggressive GC).
	if ok == 0 {
		t.Fatal("zero successful requests under cgroup-sim limits")
	}
	total := ok + errs
	if total > 0 {
		errRate := float64(errs) / float64(total)
		if errRate > 0.01 {
			t.Errorf("error rate %.3f%% exceeds 1%% under cgroup-sim limits", errRate*100)
		}
	}
	if m.HeapInuse > uint64(memLimit) {
		t.Errorf("HeapInuse (%d MiB) exceeded GOMEMLIMIT (%d MiB)",
			m.HeapInuse/(1<<20), memLimit/(1<<20))
	}
}
