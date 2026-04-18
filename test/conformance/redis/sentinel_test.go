//go:build redis_sentinel

// Package redis_test — sentinel conformance. Gated on the `redis_sentinel`
// build tag AND the CELERIS_REDIS_SENTINEL_ADDRS env var so the suite
// skips cleanly on a vanilla checkout.
//
//	docker compose -f test/conformance/redis/docker-compose.sentinel.yml up -d
//	export CELERIS_REDIS_SENTINEL_ADDRS=127.0.0.1:26379
//	export CELERIS_REDIS_SENTINEL_MASTER=mymaster
//	go test -tags redis_sentinel -race ./test/conformance/redis/...
package redis_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
)

const (
	envSentinelAddrs  = "CELERIS_REDIS_SENTINEL_ADDRS"
	envSentinelMaster = "CELERIS_REDIS_SENTINEL_MASTER"
)

func sentinelConfigFromEnv(t *testing.T) redis.SentinelConfig {
	t.Helper()
	rawAddrs := os.Getenv(envSentinelAddrs)
	if rawAddrs == "" {
		t.Skipf("skipping: %s not set", envSentinelAddrs)
	}
	master := os.Getenv(envSentinelMaster)
	if master == "" {
		master = "mymaster"
	}
	parts := strings.Split(rawAddrs, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return redis.SentinelConfig{
		MasterName:    master,
		SentinelAddrs: parts,
		DialTimeout:   5 * time.Second,
	}
}

func newSentinelClient(t *testing.T) *redis.SentinelClient {
	t.Helper()
	s, err := redis.NewSentinelClient(sentinelConfigFromEnv(t))
	if err != nil {
		t.Fatalf("NewSentinelClient: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestSentinelDiscoverAndRoundTrip proves the sentinel discovery path
// actually lands on a live master: the client asks sentinel for master
// addr via SENTINEL get-master-addr-by-name, dials that addr, and a
// trivial SET/GET round-trips through it.
func TestSentinelDiscoverAndRoundTrip(t *testing.T) {
	s := newSentinelClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	const key = "sentinel-test:rt"
	if err := s.Set(ctx, key, "ok", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "ok" {
		t.Fatalf("Get = %q, want ok", got)
	}
}

// TestSentinelMultipleKeys hits several keys to exercise the normal
// command path on the discovered master (no slot math like Cluster, but
// we still want to confirm the SentinelClient's *Client is a real
// working client and not a stub).
func TestSentinelMultipleKeys(t *testing.T) {
	s := newSentinelClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const n = 20
	for i := range n {
		if err := s.Set(ctx, fmt.Sprintf("sentinel-test:k%d", i), fmt.Sprintf("v%d", i), 0); err != nil {
			t.Fatalf("Set[%d]: %v", i, err)
		}
	}
	for i := range n {
		got, err := s.Get(ctx, fmt.Sprintf("sentinel-test:k%d", i))
		if err != nil {
			t.Fatalf("Get[%d]: %v", i, err)
		}
		if got != fmt.Sprintf("v%d", i) {
			t.Errorf("Get[%d] = %q, want v%d", i, got, i)
		}
	}
}

// TestSentinelMGet exercises a multi-key command through the sentinel
// client, proving that pipelined / multi-key ops route through the
// same master.
func TestSentinelMGet(t *testing.T) {
	s := newSentinelClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	keys := []string{"sentinel-mget:a", "sentinel-mget:b", "sentinel-mget:c"}
	for i, k := range keys {
		if err := s.Set(ctx, k, fmt.Sprintf("v%d", i), 0); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	got, err := s.MGet(ctx, keys...)
	if err != nil {
		t.Fatalf("MGet: %v", err)
	}
	if len(got) != len(keys) {
		t.Fatalf("MGet len=%d, want %d", len(got), len(keys))
	}
	for i, g := range got {
		if g != fmt.Sprintf("v%d", i) {
			t.Errorf("MGet[%d] = %q, want v%d", i, g, i)
		}
	}
}

// TestSentinelPingAfterIdle is a smoke test for the keep-alive path:
// sleep briefly, then prove the cached *Client still works.
// (A real failover test — stop the master container, wait for Sentinel
// to promote a replica, retry — is a nightly-only scenario that needs
// docker-level control beyond what this CI job has.)
func TestSentinelPingAfterIdle(t *testing.T) {
	s := newSentinelClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.Ping(ctx); err != nil {
		t.Fatalf("initial Ping: %v", err)
	}
	time.Sleep(1 * time.Second)
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("post-idle Ping: %v", err)
	}
}
