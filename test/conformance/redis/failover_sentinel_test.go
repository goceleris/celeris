//go:build redis_sentinel_failover

// Package redis_test — sentinel failover drill. Destructive: kills the
// current master container and asserts the SentinelClient reconnects to
// the promoted replica. Gated on:
//
//  1. the `redis_sentinel_failover` build tag,
//  2. CELERIS_REDIS_SENTINEL_ADDRS (existing),
//  3. the `docker` CLI on $PATH.
//
// Run:
//
//	docker compose -f test/conformance/redis/docker-compose.sentinel.yml up -d
//	export CELERIS_REDIS_SENTINEL_ADDRS=127.0.0.1:26379,127.0.0.1:26380,127.0.0.1:26381
//	export CELERIS_REDIS_SENTINEL_MASTER=mymaster
//	go test -tags redis_sentinel_failover -race -count=1 -timeout=300s \
//	  ./test/conformance/redis/...
//
// The master container is restarted on cleanup so the compose stack is
// reusable by subsequent test runs.
package redis_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
)

const (
	envSentinelAddrsFailover  = "CELERIS_REDIS_SENTINEL_ADDRS"
	envSentinelMasterFailover = "CELERIS_REDIS_SENTINEL_MASTER"
)

// newSentinelClientFailover builds a SentinelClient from the same env vars
// as the `redis_sentinel`-tagged harness. Duplicated because tag boundaries
// keep the harness helper invisible here.
func newSentinelClientFailover(t *testing.T) *redis.SentinelClient {
	t.Helper()
	rawAddrs := os.Getenv(envSentinelAddrsFailover)
	if rawAddrs == "" {
		t.Skipf("skipping: %s not set", envSentinelAddrsFailover)
	}
	master := os.Getenv(envSentinelMasterFailover)
	if master == "" {
		master = "mymaster"
	}
	parts := strings.Split(rawAddrs, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	s, err := redis.NewSentinelClient(redis.SentinelConfig{
		MasterName:    master,
		SentinelAddrs: parts,
		DialTimeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSentinelClient: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestSentinelFailoverReadsFollow writes a canary key, kills the current
// master container, and polls Get on the canary until it succeeds. The
// SentinelClient listens to +switch-master and should reconnect to the
// promoted replica — if it doesn't, reads never recover and the test
// fails by timeout.
//
// Sentinel down-after-milliseconds is 5000ms in docker-compose.sentinel.yml
// and failover-timeout is 10000ms, so expected recovery lands around
// 12-20s after the stop. We poll for 60s to absorb CI jitter.
func TestSentinelFailoverReadsFollow(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("skipping: docker CLI not available")
	}

	s := newSentinelClientFailover(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Seed a canary key so we have something to probe after failover. A
	// promoted replica will have the canary (it was replicating from the
	// old master when we wrote it) — the invariant is that the driver
	// finds the new master and the key is readable through it.
	const canary = "sentinel-failover:canary"
	if err := s.Set(ctx, canary, "alive", 0); err != nil {
		t.Fatalf("seed Set: %v", err)
	}
	if got, err := s.Get(ctx, canary); err != nil || got != "alive" {
		t.Fatalf("pre-kill Get = (%q, %v), want (\"alive\", nil)", got, err)
	}

	// Bring the old master back on cleanup so the stack is reusable.
	// Sentinel will reconfigure it as a replica of the promoted node —
	// that's fine; we don't need to un-failover, just to keep all
	// containers alive.
	t.Cleanup(func() {
		cmd := exec.Command("docker", "start", "celeris-sentinel-master")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("docker start celeris-sentinel-master (cleanup): %v: %s", err, string(out))
		}
	})

	t.Logf("stopping celeris-sentinel-master to trigger failover")
	if out, err := exec.Command("docker", "stop", "celeris-sentinel-master").CombinedOutput(); err != nil {
		t.Fatalf("docker stop celeris-sentinel-master: %v: %s", err, string(out))
	}

	// Poll Get for up to 60s. Any success is a pass — the driver has
	// rediscovered the master via sentinel and the canary value is
	// readable through the new primary connection.
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	var got string
	attempts := 0
	for time.Now().Before(deadline) {
		attempts++
		getCtx, getCancel := context.WithTimeout(ctx, 3*time.Second)
		got, lastErr = s.Get(getCtx, canary)
		getCancel()
		if lastErr == nil {
			t.Logf("Get recovered after %d attempts (~%s)", attempts, time.Duration(attempts)*500*time.Millisecond)
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("Get %s never recovered after master kill (attempted %d times in 60s): %v",
			canary, attempts, lastErr)
	}
	if got != "alive" {
		t.Errorf("Get %s = %q, want \"alive\" (promoted replica should have the seeded value)", canary, got)
	}
}
