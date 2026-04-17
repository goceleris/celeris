//go:build redis_cluster_failover

// Package redis_test — cluster failover drills. Destructive: tests kill a
// master container mid-run and assert the driver recovers. Gated on:
//
//  1. the `redis_cluster_failover` build tag (off by default because these
//     tests tear apart the cluster),
//  2. CELERIS_REDIS_CLUSTER_ADDRS (existing),
//  3. the `docker` CLI on $PATH (skipped if absent — the tests use
//     `docker stop` / `docker start` to trigger failover).
//
// Run:
//
//	docker compose -f test/conformance/redis/docker-compose.cluster.yml up -d
//	export CELERIS_REDIS_CLUSTER_ADDRS=127.0.0.1:7000,127.0.0.1:7001,127.0.0.1:7002
//	go test -tags redis_cluster_failover -race -count=1 -timeout=300s \
//	  ./test/conformance/redis/...
//
// Each test restores the killed container via t.Cleanup so the suite leaves
// the cluster reusable by any follow-up conformance run.
package redis_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
)

// envClusterAddrsFailover — same name as the const in the `redis_cluster`
// harness but scoped to this file's build tag (the two never compile
// together so there is no collision).
const envClusterAddrsFailover = "CELERIS_REDIS_CLUSTER_ADDRS"

// clusterAddrsFromEnvFailover resolves CELERIS_REDIS_CLUSTER_ADDRS or skips.
// Duplicated from the `redis_cluster`-tagged harness because the failover
// suite compiles under its own tag — the helper there isn't visible here.
func clusterAddrsFromEnvFailover(t *testing.T) []string {
	t.Helper()
	raw := os.Getenv(envClusterAddrsFailover)
	if raw == "" {
		t.Skipf("skipping: %s not set", envClusterAddrsFailover)
	}
	parts := strings.Split(raw, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

// newClusterClientFailover mirrors newClusterClient in cluster_test.go. See
// the comment on clusterAddrsFromEnvFailover for the duplication rationale.
func newClusterClientFailover(t *testing.T) *redis.ClusterClient {
	t.Helper()
	addrs := clusterAddrsFromEnvFailover(t)
	c, err := redis.NewClusterClient(redis.ClusterConfig{
		Addrs:       addrs,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// dockerAvailable reports whether the docker CLI is on $PATH. Failover tests
// skip when false — a dev laptop without docker should stay green.
func dockerAvailable(t *testing.T) bool {
	t.Helper()
	_, err := exec.LookPath("docker")
	return err == nil
}

// dockerStop stops a container, returning any error verbatim.
func dockerStop(t *testing.T, name string) error {
	t.Helper()
	cmd := exec.Command("docker", "stop", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker stop %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dockerStart starts a stopped container. Best-effort — we log rather than
// fail so a cleanup chain doesn't mask the original test error.
func dockerStart(t *testing.T, name string) {
	t.Helper()
	cmd := exec.Command("docker", "start", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Logf("docker start %s (cleanup): %v: %s", name, err, strings.TrimSpace(string(out)))
	}
}

// waitClusterHealthy polls `redis-cli cluster info` on a surviving node
// until cluster_state:ok or until the deadline. Used after bringing a
// stopped master back, so downstream tests see a quorate cluster.
func waitClusterHealthy(t *testing.T, surviving string, deadline time.Time) {
	t.Helper()
	for time.Now().Before(deadline) {
		cmd := exec.Command("docker", "exec", surviving, "redis-cli", "cluster", "info")
		out, err := cmd.CombinedOutput()
		if err == nil && strings.Contains(string(out), "cluster_state:ok") {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("warning: cluster never re-reached state=ok before deadline; leaving as-is")
}

// TestClusterFailoverMasterKill writes 20 keys, picks one whose slot lives on
// port 7000, kills celeris-cluster-0 via `docker stop`, then polls Get on
// that key for up to 30s. The replica is promoted and the driver's MOVED +
// topology-refresh path must route subsequent reads to the new master.
//
// This is the headline "driver survives a dead master" claim — before this
// test, only the happy-path MOVED regression test in driver/redis
// covered it, and that ran against an in-process stub that always responded.
// Here the master really dies.
func TestClusterFailoverMasterKill(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("skipping: docker CLI not available")
	}
	c := newClusterClientFailover(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// Write 20 keys spread across the ring.
	keys := make([]string, 20)
	for i := range keys {
		keys[i] = fmt.Sprintf("cluster-failover-kill:k%d", i)
		if err := c.Set(ctx, keys[i], fmt.Sprintf("v%d", i), 0); err != nil {
			t.Fatalf("seed Set %s: %v", keys[i], err)
		}
	}

	// Pick the key whose slot lands on port 7000's shard. CLUSTER KEYSLOT
	// tells us the slot; the driver's own routing tells us which node
	// currently owns that slot. We use `redis-cli -p 7000 cluster
	// countkeysinslot N` to decide whether 7000 owns the slot. Simpler:
	// just iterate until we find a key whose SET to 7000 succeeded without
	// MOVED — that's signal enough that 7000 owns it.
	var killKey string
	for _, k := range keys {
		slot := redis.Slot(k)
		cmd := exec.Command("docker", "exec", "celeris-cluster-0", "redis-cli", "-p", "7000",
			"cluster", "countkeysinslot", fmt.Sprintf("%d", slot))
		out, err := cmd.CombinedOutput()
		if err != nil {
			continue
		}
		// Positive count means slot lives on 7000 (we already SET the key).
		if strings.TrimSpace(string(out)) != "0" {
			killKey = k
			break
		}
	}
	if killKey == "" {
		t.Skip("skipping: no test key landed on celeris-cluster-0 — shard layout avoided this node")
	}

	// Pre-check: Get works before the kill.
	if _, err := c.Get(ctx, killKey); err != nil {
		t.Fatalf("pre-kill Get %s: %v", killKey, err)
	}

	// Make sure we bring the node back even if the test fails or times out.
	// Run t.Cleanup even if the stop step below errors.
	t.Cleanup(func() {
		dockerStart(t, "celeris-cluster-0")
		// Give the cluster ~20s to re-stabilize after readmit. If the node
		// rejoins as a replica of the promoted former-replica, that's fine —
		// cluster_state:ok is the invariant we need.
		waitClusterHealthy(t, "celeris-cluster-1", time.Now().Add(20*time.Second))
	})

	t.Logf("killing celeris-cluster-0; target key=%q (slot=%d)", killKey, redis.Slot(killKey))
	if err := dockerStop(t, "celeris-cluster-0"); err != nil {
		t.Fatalf("docker stop: %v", err)
	}

	// Poll Get for up to 30s. The cluster-node-timeout in docker-compose is
	// 5000ms so a promoted replica should be reachable within ~6-8s once
	// the driver's topology map catches up. Fast recovery depends on the
	// driver treating a network error on a primary as a topology-refresh
	// trigger; see the v1.4.0 gap-closure report for the driver-level
	// tracking of this invariant.
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	var got string
	for time.Now().Before(deadline) {
		getCtx, getCancel := context.WithTimeout(ctx, 3*time.Second)
		got, lastErr = c.Get(getCtx, killKey)
		getCancel()
		if lastErr == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("Get %s never recovered after master kill: %v", killKey, lastErr)
	}

	// Any one of the other keys on the same dead node should also be
	// reachable now that the topology has refreshed.
	if want := "v"; !strings.HasPrefix(got, want) {
		t.Errorf("Get %s = %q, expected 'v<n>' prefix (value should survive failover)", killKey, got)
	}
}

// TestClusterFailoverPipelineSurvives issues a 50-command pipeline while a
// master is being killed in a background goroutine. The driver is expected
// to either:
//
//   - complete the whole pipeline (if topology refreshed in time), OR
//   - return errors on the affected commands that identify as MOVED /
//     CLUSTERDOWN / ERR — i.e. explicit failure, not silent wrong data.
//
// The critical invariant is NO silent wrong replies: every successful result
// must match the expected value, even though some commands may fail.
func TestClusterFailoverPipelineSurvives(t *testing.T) {
	if !dockerAvailable(t) {
		t.Skip("skipping: docker CLI not available")
	}
	c := newClusterClientFailover(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const n = 50
	// Pre-seed: SET every key synchronously so we have a known ground truth.
	// Use cross-slot keys (no hash tag) so the pipeline really fans across
	// shards — a single-slot pipeline would all-or-nothing on one node and
	// wouldn't exercise the per-node topology churn we care about.
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("cluster-failover-pipe:k%d", i)
		if err := c.Set(ctx, keys[i], fmt.Sprintf("v%d", i), 0); err != nil {
			t.Fatalf("seed Set %s: %v", keys[i], err)
		}
	}

	t.Cleanup(func() {
		dockerStart(t, "celeris-cluster-1")
		waitClusterHealthy(t, "celeris-cluster-0", time.Now().Add(20*time.Second))
	})

	// Kick off the kill after 100ms so the pipeline is mid-flight.
	killDone := make(chan struct{})
	go func() {
		defer close(killDone)
		time.Sleep(100 * time.Millisecond)
		if err := dockerStop(t, "celeris-cluster-1"); err != nil {
			t.Logf("docker stop (background): %v", err)
		}
	}()

	// Build the pipeline: GET every seeded key.
	p := c.Pipeline()
	idx := make([]int, n)
	for i := range n {
		idx[i] = p.Get(keys[i])
	}

	pipeCtx, pipeCancel := context.WithTimeout(ctx, 15*time.Second)
	results, errs := p.Exec(pipeCtx)
	pipeCancel()

	if len(results) != n {
		<-killDone
		t.Fatalf("Exec returned %d results, want %d", len(results), n)
	}

	// Walk results: count successes, failures, and — critically — any
	// silent wrong replies. A mismatched value on a nil-error slot is the
	// failure mode we're hunting.
	var (
		ok, failed, wrongData int
	)
	for i := range n {
		err := errs[idx[i]]
		if err != nil {
			failed++
			// Classify the error: MOVED / CLUSTERDOWN / ERR / network reset
			// are all acceptable. Silent data corruption is not.
			var rerr *redis.RedisError
			if errors.As(err, &rerr) {
				switch rerr.Prefix {
				case "MOVED", "ASK", "CLUSTERDOWN", "TRYAGAIN", "ERR":
					// expected during a master kill
				default:
					t.Errorf("Get[%d] unexpected Redis error prefix %q: %v", i, rerr.Prefix, err)
				}
			}
			// Non-Redis errors (ECONNRESET, ETIMEDOUT, deadline exceeded,
			// "no cluster nodes available") are also expected during kill.
			continue
		}
		want := fmt.Sprintf("v%d", i)
		got := string(results[idx[i]].Str)
		if got != want {
			wrongData++
			t.Errorf("Get[%d] SILENT WRONG DATA: got %q, want %q (driver returned stale/misrouted reply)", i, got, want)
		} else {
			ok++
		}
	}
	<-killDone
	t.Logf("pipeline outcome: %d ok, %d failed, %d WRONG DATA (must be 0)", ok, failed, wrongData)
	if wrongData > 0 {
		t.Fatalf("pipeline produced %d silent-wrong-data replies — driver correctness bug", wrongData)
	}
}
