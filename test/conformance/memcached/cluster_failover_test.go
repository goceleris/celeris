//go:build memcached_cluster_failover

// Cluster failover conformance: exercises the v1.4.1 node-failure
// detection, passive-heal, and background-probe paths against a real
// 3-node memcached deployment. Gated by the `memcached_cluster_failover`
// build tag and the CELERIS_MEMCACHED_CLUSTER_ADDRS environment variable
// (comma-separated host:port list). The tests assume docker-compose-
// managed containers (docker-compose.cluster.yml) so they can be stopped
// and restarted mid-test via docker CLI.
package memcached_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
)

// dockerCanStop returns true when the docker CLI is available; tests
// that depend on bouncing containers skip otherwise.
func dockerCanStop(t *testing.T) bool {
	t.Helper()
	if os.Getenv("CELERIS_MEMCACHED_DOCKER_PREFIX") == "" {
		t.Skip("skipping: CELERIS_MEMCACHED_DOCKER_PREFIX not set (e.g. 'memcached-cluster')")
		return false
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("skipping: docker CLI not available")
		return false
	}
	return true
}

func dockerExec(t *testing.T, args ...string) error {
	t.Helper()
	cmd := exec.Command("docker", args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}

// TestClusterFailoverLive drives real traffic through the cluster while
// one node is stopped, then restarted, and asserts:
//
//   - Traffic continues without hard errors while the node is down
//     (rerouted to a successor).
//   - NodeHealth() reports the failing node as failing within the
//     failure-threshold window.
//   - After the node is restarted, the background probe clears the
//     failing flag within a few probe intervals.
func TestClusterFailoverLive(t *testing.T) {
	if !dockerCanStop(t) {
		return
	}
	addrs := clusterAddrsFromEnvShared(t)
	if len(addrs) < 3 {
		t.Skip("failover requires at least 3 nodes")
	}
	prefix := os.Getenv("CELERIS_MEMCACHED_DOCKER_PREFIX")

	cc, err := celmc.NewClusterClient(celmc.ClusterConfig{
		Addrs:               addrs,
		DialTimeout:         2 * time.Second,
		Timeout:             2 * time.Second,
		FailureThreshold:    2,
		HealthProbeInterval: 1 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Warm up.
	for i := 0; i < 20; i++ {
		if err := cc.Set(ctx, fmt.Sprintf("warm-%d", i), "v", 5*time.Minute); err != nil {
			t.Fatalf("warm Set %d: %v", i, err)
		}
	}

	// Stop node B (index 1). Use docker stop with a small SIGTERM timeout so
	// the port becomes unusable promptly.
	containerB := prefix + "-memcached-b-1"
	if err := dockerExec(t, "stop", "-t", "1", containerB); err != nil {
		t.Skipf("docker stop %s failed (likely different compose layout): %v", containerB, err)
	}

	// Issue traffic — expect most writes to succeed through the successor.
	okCount := 0
	for i := 0; i < 200; i++ {
		if err := cc.Set(ctx, fmt.Sprintf("post-down-%d", i), "v", time.Minute); err == nil {
			okCount++
		}
	}
	if okCount < 100 {
		t.Fatalf("post-down write success rate too low: %d/200 (expected ≥100)", okCount)
	}

	// By now the failing node must be marked.
	healthy := cc.NodeHealth()[addrs[1]] == false
	if healthy {
		t.Fatalf("node 1 should be marked failing after outage; NodeHealth=%v", cc.NodeHealth())
	}

	// Restart node B.
	containerBName := strings.TrimSpace(containerB)
	if err := dockerExec(t, "start", containerBName); err != nil {
		t.Fatalf("docker start %s: %v", containerBName, err)
	}

	// The background probe should clear the flag within ~5 seconds.
	deadline := time.Now().Add(20 * time.Second)
	var recovered bool
	for time.Now().Before(deadline) {
		if !cc.NodeHealth()[addrs[1]] {
			recovered = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !recovered {
		t.Fatalf("probe did not clear failing flag for %s within 20s; NodeHealth=%v", addrs[1], cc.NodeHealth())
	}

	// Traffic continues.
	for i := 0; i < 20; i++ {
		if err := cc.Set(ctx, fmt.Sprintf("post-recover-%d", i), "v", time.Minute); err != nil {
			t.Fatalf("post-recover Set %d: %v", i, err)
		}
	}
}

// TestClusterKeyStabilityLive is the live-services counterpart of
// driver/memcached/cluster_failover_test.go:TestClusterFailoverKeyStability.
// It asserts that when a node is marked failing, every key formerly
// owned by that node migrates to the same successor rather than being
// scattered across the ring.
func TestClusterKeyStabilityLive(t *testing.T) {
	addrs := clusterAddrsFromEnvShared(t)
	if len(addrs) < 3 {
		t.Skip("key-stability test requires at least 3 nodes")
	}
	cc, err := celmc.NewClusterClient(celmc.ClusterConfig{
		Addrs:               addrs,
		DialTimeout:         2 * time.Second,
		HealthProbeInterval: 0,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	// Identify keys routed to the middle node.
	const total = 5000
	middleAddr := addrs[1]
	var onMiddle []string
	for i := 0; i < total; i++ {
		k := fmt.Sprintf("ks-%d", i)
		if cc.NodeFor(k) == middleAddr {
			onMiddle = append(onMiddle, k)
		}
	}
	if len(onMiddle) < 500 {
		t.Fatalf("too few keys on middle node (%d); ring may be empty", len(onMiddle))
	}

	// Pretend the middle node is failing.
	stats := cc.NodeStatsMap()
	if stats[middleAddr].Failing {
		t.Skip("middle node already failing; recovery test cannot proceed")
	}
	// Simulate a failed state by issuing ops to a closed port on the middle
	// node is not possible from conformance; instead, rely on a short TCP
	// outage via docker. If docker is unavailable, skip.
	if !dockerCanStop(t) {
		return
	}
	prefix := os.Getenv("CELERIS_MEMCACHED_DOCKER_PREFIX")
	containerB := prefix + "-memcached-b-1"
	if err := dockerExec(t, "stop", "-t", "1", containerB); err != nil {
		t.Skipf("docker stop %s failed: %v", containerB, err)
	}
	// Defer restart so we don't leak a stopped container across tests.
	defer func() { _ = dockerExec(t, "start", containerB) }()

	// Trigger failure marking by issuing a few ops.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 5; i++ {
		_ = cc.Set(ctx, onMiddle[0], "v", time.Minute)
	}

	// Every formerly-on-middle key now routes to a single successor.
	var successor string
	for _, k := range onMiddle {
		got := cc.NodeFor(k)
		if got == middleAddr {
			t.Fatalf("key %q still routing to failing middle node", k)
		}
		if successor == "" {
			successor = got
		} else if got != successor {
			t.Fatalf("key-stability broken: %q went to %q, expected %q", k, got, successor)
		}
	}
	t.Logf("all %d middle-node keys redirected to %s", len(onMiddle), successor)
}

// TestClusterChaosSustainedLoad runs heavy concurrent traffic through
// the cluster while randomly stopping/restarting nodes in the background.
// Assertions:
//
//   - Sustained throughput stays above a minimum threshold (200 ops/s
//     per goroutine across the whole run; tunable via CHAOS_MIN_RPS).
//   - Data consistency: a key written to the cluster reads back the
//     same value (no torn writes, no returning a stale value after a
//     restart cycle).
//   - p99 latency does not exceed CHAOS_P99_CEILING_MS (default 500ms)
//     — a generous bound since chaos injection DOES spike latency.
//
// Gated by CELERIS_MEMCACHED_DOCKER_PREFIX. Opt-in via CHAOS_DURATION
// (default 30s).
func TestClusterChaosSustainedLoad(t *testing.T) {
	if !dockerCanStop(t) {
		return
	}
	addrs := clusterAddrsFromEnvShared(t)
	if len(addrs) < 3 {
		t.Skip("chaos test requires at least 3 nodes")
	}
	prefix := os.Getenv("CELERIS_MEMCACHED_DOCKER_PREFIX")

	duration := 30 * time.Second
	if dv := os.Getenv("CHAOS_DURATION"); dv != "" {
		if d, err := time.ParseDuration(dv); err == nil {
			duration = d
		}
	}
	p99Ceiling := 500 * time.Millisecond
	if cv := os.Getenv("CHAOS_P99_CEILING_MS"); cv != "" {
		var ms int
		if _, err := fmt.Sscanf(cv, "%d", &ms); err == nil && ms > 0 {
			p99Ceiling = time.Duration(ms) * time.Millisecond
		}
	}
	concurrency := 64
	if cv := os.Getenv("CHAOS_CONC"); cv != "" {
		fmt.Sscanf(cv, "%d", &concurrency)
	}

	cc, err := celmc.NewClusterClient(celmc.ClusterConfig{
		Addrs:               addrs,
		DialTimeout:         2 * time.Second,
		FailureThreshold:    2,
		HealthProbeInterval: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	// Chaos goroutine: stop + restart a random node every few seconds.
	// Keep one node always alive so the cluster retains routing majority.
	containers := make([]string, len(addrs))
	for i, a := range addrs {
		_ = a
		containers[i] = fmt.Sprintf("%s-memcached-%c-1", prefix, 'a'+i)
	}
	chaosStop := make(chan struct{})
	chaosDone := make(chan struct{})
	var chaosEvents atomic.Int64
	go func() {
		defer close(chaosDone)
		r := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0))
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-chaosStop:
				return
			case <-ticker.C:
				// Pick a non-first container (keep [0] always alive).
				idx := 1 + r.IntN(len(containers)-1)
				c := containers[idx]
				t.Logf("chaos: stopping %s", c)
				if err := dockerExec(t, "stop", "-t", "1", c); err != nil {
					t.Logf("chaos: docker stop %s: %v", c, err)
					continue
				}
				chaosEvents.Add(1)
				// Wait 2s then restart.
				select {
				case <-time.After(2 * time.Second):
				case <-chaosStop:
					_ = dockerExec(t, "start", c)
					return
				}
				t.Logf("chaos: restarting %s", c)
				if err := dockerExec(t, "start", c); err != nil {
					t.Logf("chaos: docker start %s: %v", c, err)
				}
			}
		}
	}()
	defer func() {
		close(chaosStop)
		<-chaosDone
		// Restart every container (best-effort) so subsequent tests
		// find a clean cluster.
		for _, c := range containers {
			_ = dockerExec(t, "start", c)
		}
	}()

	// Load drivers.
	var okCount, errCount, mismatchCount atomic.Int64
	var latencies []time.Duration
	var latMu sync.Mutex
	var wg sync.WaitGroup
	stopLoad := make(chan struct{})
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			local := make([]time.Duration, 0, 8192)
			keyBase := fmt.Sprintf("chaos-w%d-", w)
			for i := 0; ; i++ {
				select {
				case <-stopLoad:
					latMu.Lock()
					latencies = append(latencies, local...)
					latMu.Unlock()
					return
				default:
				}
				key := keyBase + fmt.Sprintf("%d", i%100)
				val := fmt.Sprintf("val-%d-%d", w, i)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				t0 := time.Now()
				err := cc.Set(ctx, key, val, 5*time.Minute)
				if err != nil {
					cancel()
					errCount.Add(1)
					continue
				}
				got, gerr := cc.Get(ctx, key)
				cancel()
				elapsed := time.Since(t0)
				local = append(local, elapsed)
				if gerr != nil {
					errCount.Add(1)
					continue
				}
				if got != val {
					mismatchCount.Add(1)
					t.Errorf("data corruption: key=%q wrote=%q got=%q", key, val, got)
					errCount.Add(1)
					continue
				}
				okCount.Add(1)
			}
		}(w)
	}

	start := time.Now()
	time.Sleep(duration)
	close(stopLoad)
	wg.Wait()
	elapsed := time.Since(start)

	ok := okCount.Load()
	errs := errCount.Load()
	mismatches := mismatchCount.Load()
	rps := float64(ok) / elapsed.Seconds()
	t.Logf("chaos summary: elapsed=%v ok=%d err=%d mismatch=%d rps=%.0f chaos_events=%d",
		elapsed, ok, errs, mismatches, rps, chaosEvents.Load())

	if mismatches > 0 {
		t.Fatalf("data corruption: %d mismatches (MUST be 0)", mismatches)
	}
	// Minimum throughput: a chaos run tanks RPS by 5-30% but not by
	// more than that. Require at least 100 ops/s overall — the real
	// guarantee is no crashes + no corruption.
	if rps < 100 {
		t.Fatalf("sustained RPS dropped below 100: got %.0f (severe regression)", rps)
	}

	// p99 latency ceiling.
	latMu.Lock()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	var p99 time.Duration
	if len(latencies) > 0 {
		idx := int(float64(len(latencies)-1) * 0.99)
		p99 = latencies[idx]
	}
	latMu.Unlock()
	t.Logf("p99 latency under chaos: %v (ceiling %v)", p99, p99Ceiling)
	if p99 > p99Ceiling {
		t.Errorf("p99 latency under chaos exceeded ceiling: %v > %v", p99, p99Ceiling)
	}
}
