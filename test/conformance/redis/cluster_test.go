//go:build redis_cluster

// Package redis_test — cluster conformance. Gated on the `redis_cluster`
// build tag AND the CELERIS_REDIS_CLUSTER_ADDRS env var so the suite skips
// cleanly on a vanilla checkout. Start a real cluster via the matching
// docker-compose.cluster.yml in this directory.
//
//	docker compose -f test/conformance/redis/docker-compose.cluster.yml up -d
//	export CELERIS_REDIS_CLUSTER_ADDRS=127.0.0.1:7000,127.0.0.1:7001,127.0.0.1:7002
//	go test -tags redis_cluster -race ./test/conformance/redis/...
package redis_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/driver/redis/protocol"
)

const envClusterAddrs = "CELERIS_REDIS_CLUSTER_ADDRS"

func clusterAddrsFromEnv(t *testing.T) []string {
	t.Helper()
	raw := os.Getenv(envClusterAddrs)
	if raw == "" {
		t.Skipf("skipping: %s not set", envClusterAddrs)
	}
	parts := strings.Split(raw, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

func newClusterClient(t *testing.T) *redis.ClusterClient {
	t.Helper()
	addrs := clusterAddrsFromEnv(t)
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

// TestClusterSetGetAcrossSlots covers the base functionality: store + fetch
// keys spanning different hash slots. Proves MOVED redirection or correct
// initial slot routing — whichever the client takes — delivers values to
// the right node.
func TestClusterSetGetAcrossSlots(t *testing.T) {
	c := newClusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	keys := []string{
		"cluster-test:alpha",
		"cluster-test:beta",
		"cluster-test:gamma",
		"cluster-test:delta",
		"cluster-test:epsilon",
		"cluster-test:zeta",
		"cluster-test:eta",
		"cluster-test:theta",
	}
	for i, k := range keys {
		want := fmt.Sprintf("v%d", i)
		if err := c.Set(ctx, k, want, 0); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	for i, k := range keys {
		got, err := c.Get(ctx, k)
		if err != nil {
			t.Fatalf("Get %s: %v", k, err)
		}
		if got != fmt.Sprintf("v%d", i) {
			t.Errorf("Get %s = %q, want v%d", k, got, i)
		}
	}
}

// TestClusterHashTagsPinToOneSlot verifies that `{tag}`-wrapped keys all
// hash to the same slot. Exercises the slot-derivation path used by
// Pipeline.execRound to group commands by slot.
func TestClusterHashTagsPinToOneSlot(t *testing.T) {
	c := newClusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const tag = "{cluster-tag-test}"
	keys := []string{tag + ":k1", tag + ":k2", tag + ":k3"}
	slot0 := redis.Slot(keys[0])
	for _, k := range keys[1:] {
		if s := redis.Slot(k); s != slot0 {
			t.Fatalf("Slot(%q) = %d, want %d (hash tags broken)", k, s, slot0)
		}
	}
	for i, k := range keys {
		if err := c.Set(ctx, k, fmt.Sprintf("v%d", i), 0); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	for i, k := range keys {
		got, err := c.Get(ctx, k)
		if err != nil {
			t.Fatalf("Get %s: %v", k, err)
		}
		if got != fmt.Sprintf("v%d", i) {
			t.Errorf("Get %s = %q, want v%d", k, got, i)
		}
	}
}

// TestClusterPipelineSameSlot exercises the single-slot pipeline path,
// which batches every queued command on one conn. ClusterPipeline.Exec
// is the code path most recently patched (commit 299d01c — silently
// dropped replies before fix).
func TestClusterPipelineSameSlot(t *testing.T) {
	c := newClusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const tag = "{cluster-pipeline}"
	p := c.Pipeline()
	const n = 5
	setIdx := make([]int, n)
	for i := range n {
		setIdx[i] = p.Set(tag+fmt.Sprintf(":k%d", i), fmt.Sprintf("v%d", i), 0)
	}
	getIdx := make([]int, n)
	for i := range n {
		getIdx[i] = p.Get(tag + fmt.Sprintf(":k%d", i))
	}

	results, errs := p.Exec(ctx)
	if len(results) != 2*n {
		t.Fatalf("Exec returned %d results, want %d", len(results), 2*n)
	}
	for _, idx := range setIdx {
		if errs[idx] != nil {
			t.Fatalf("Set[%d]: %v", idx, errs[idx])
		}
		if results[idx].Type != protocol.TySimple && results[idx].Type != protocol.TyBulk {
			t.Errorf("Set[%d]: got type=%v str=%q, want OK status", idx, results[idx].Type, results[idx].Str)
		}
	}
	for i, idx := range getIdx {
		if errs[idx] != nil {
			t.Fatalf("Get[%d]: %v", idx, errs[idx])
		}
		want := fmt.Sprintf("v%d", i)
		if got := string(results[idx].Str); got != want {
			t.Errorf("Get[%d]: got %q, want %q", idx, got, want)
		}
	}
}

// TestClusterPipelineCrossSlot scatters commands across multiple slots and
// verifies that the client fans out to the right nodes and stitches
// replies back in caller order — this is where MOVED/ASK retries most
// visibly regress if topology logic breaks.
func TestClusterPipelineCrossSlot(t *testing.T) {
	c := newClusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	keys := []string{
		"cluster-pipe:a", "cluster-pipe:b", "cluster-pipe:c",
		"cluster-pipe:d", "cluster-pipe:e", "cluster-pipe:f",
	}
	p := c.Pipeline()
	setIdx := make([]int, len(keys))
	for i, k := range keys {
		setIdx[i] = p.Set(k, fmt.Sprintf("v%d", i), 0)
	}
	getIdx := make([]int, len(keys))
	for i, k := range keys {
		getIdx[i] = p.Get(k)
	}
	results, errs := p.Exec(ctx)
	if len(results) != 2*len(keys) {
		t.Fatalf("Exec returned %d results, want %d", len(results), 2*len(keys))
	}
	for _, idx := range setIdx {
		if errs[idx] != nil {
			t.Fatalf("Set[%d]: %v", idx, errs[idx])
		}
	}
	for i, idx := range getIdx {
		if errs[idx] != nil {
			t.Fatalf("Get[%d]: %v", idx, errs[idx])
		}
		want := fmt.Sprintf("v%d", i)
		if got := string(results[idx].Str); got != want {
			t.Errorf("Get[%d]: got %q, want %q", idx, got, want)
		}
	}
}

// TestClusterTopologySmoke is a smoke check: the client is live, reads and
// writes work. A full failover-triggered refresh test needs container-level
// control (kill a master mid-run, wait for replica promotion, verify reads
// continue) — that's tracked as a nightly / manual-only scenario.
func TestClusterTopologySmoke(t *testing.T) {
	c := newClusterClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := c.Set(ctx, "cluster-smoke:key", "ok", 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := c.Get(ctx, "cluster-smoke:key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "ok" {
		t.Fatalf("got %q, want ok", got)
	}
}
