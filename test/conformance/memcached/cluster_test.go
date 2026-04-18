//go:build memcached_cluster

// Cluster conformance: exercises the [celmc.ClusterClient] against a real
// multi-node memcached deployment. Gated by both the `memcached_cluster`
// build tag and the CELERIS_MEMCACHED_CLUSTER_ADDRS environment variable
// (comma-separated list of host:port endpoints). Skips cleanly when either
// is absent.
//
// See docker-compose.cluster.yml for a 3-node docker stack.
package memcached_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	celmc "github.com/goceleris/celeris/driver/memcached"
)

// clusterKeyCounter gives every generated key a monotonic suffix so parallel
// subtests never collide even with the same random prefix. Kept local to the
// memcached_cluster build so it does not conflict with keyCounter in
// harness_test.go (which is gated on `memcached`).
var clusterKeyCounter uint64

// uniqueKey returns a collision-free key scoped to a test. Mirrors the helper
// in harness_test.go; duplicated rather than shared so the single-server and
// cluster conformance suites can be built under mutually-exclusive build tags
// without dragging in each other's unused helpers.
func uniqueKey(t *testing.T, label string) string {
	t.Helper()
	h := fnv.New32a()
	_, _ = h.Write([]byte(t.Name()))
	nameHash := h.Sum32()

	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	n := atomic.AddUint64(&clusterKeyCounter, 1)
	label = sanitizeLabelCluster(label)
	return fmt.Sprintf("celeris_cluster_test_%08x_%d_%s_%s", nameHash, n, hex.EncodeToString(buf), label)
}

// sanitizeLabelCluster normalizes a human label into a key-safe suffix.
func sanitizeLabelCluster(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "k"
	}
	return b.String()
}

const envClusterAddrs = "CELERIS_MEMCACHED_CLUSTER_ADDRS"

// clusterAddrsFromEnv returns the parsed list of cluster addresses or skips
// the test when the env is unset or empty.
func clusterAddrsFromEnv(t *testing.T) []string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(envClusterAddrs))
	if raw == "" {
		t.Skipf("skipping: %s not set (comma-separated host:port list)", envClusterAddrs)
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		t.Skipf("skipping: %s had no non-empty entries", envClusterAddrs)
	}
	return out
}

func newCluster(t *testing.T) *celmc.ClusterClient {
	t.Helper()
	addrs := clusterAddrsFromEnv(t)
	cc, err := celmc.NewClusterClient(celmc.ClusterConfig{
		Addrs:       addrs,
		DialTimeout: 3 * time.Second,
		Timeout:     3 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient(%v): %v", addrs, err)
	}
	// Smoke-test every node via Ping; skip when any is unreachable to avoid
	// false failures on half-warm topologies.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cc.Ping(ctx); err != nil {
		_ = cc.Close()
		t.Skipf("skipping: Ping across cluster failed: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

func TestRealClusterSetGet(t *testing.T) {
	cc := newCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const n = 50
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = uniqueKey(t, fmt.Sprintf("clr_%d", i))
		if err := cc.Set(ctx, keys[i], fmt.Sprintf("val-%d", i), 30*time.Second); err != nil {
			t.Fatalf("Set(%s): %v", keys[i], err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, k := range keys {
			_ = cc.Delete(cleanupCtx, k)
		}
	})
	for i, k := range keys {
		got, err := cc.Get(ctx, k)
		if err != nil {
			t.Fatalf("Get(%s): %v", k, err)
		}
		if want := fmt.Sprintf("val-%d", i); got != want {
			t.Fatalf("Get(%s) = %q, want %q", k, got, want)
		}
	}

	// Ensure keys actually land across >1 node — NodeFor returns the
	// computed owner; with 50 random keys across 3 nodes we expect every
	// node to own at least one.
	seen := map[string]int{}
	for _, k := range keys {
		seen[cc.NodeFor(k)]++
	}
	if len(seen) < 2 {
		t.Errorf("expected keys to spread across >1 node; owners=%v", seen)
	}
}

func TestRealClusterGetMulti(t *testing.T) {
	cc := newCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const n = 20
	keys := make([]string, n)
	want := make(map[string]string, n)
	for i := 0; i < n; i++ {
		keys[i] = uniqueKey(t, fmt.Sprintf("gm_%d", i))
		v := fmt.Sprintf("gm-val-%d", i)
		want[keys[i]] = v
		if err := cc.Set(ctx, keys[i], v, 30*time.Second); err != nil {
			t.Fatalf("Set(%s): %v", keys[i], err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, k := range keys {
			_ = cc.Delete(cleanupCtx, k)
		}
	})

	got, err := cc.GetMulti(ctx, keys...)
	if err != nil {
		t.Fatalf("GetMulti: %v", err)
	}
	if len(got) != n {
		t.Fatalf("GetMulti len=%d, want %d; missing keys=%v", len(got), n, diffKeys(want, got))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("GetMulti[%s] = %q, want %q", k, got[k], v)
		}
	}
}

func TestRealClusterStats(t *testing.T) {
	cc := newCluster(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stats, err := cc.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	addrs := cc.Addrs()
	if len(stats) != len(addrs) {
		t.Fatalf("Stats returned %d entries, want %d (addrs=%v)", len(stats), len(addrs), addrs)
	}
	for _, a := range addrs {
		s, ok := stats[a]
		if !ok {
			t.Errorf("Stats missing entry for %s", a)
			continue
		}
		if s["version"] == "" && s["pid"] == "" {
			t.Errorf("Stats[%s] looks empty: %v", a, s)
		}
	}
}

// diffKeys returns the keys of want that are missing from got; used solely
// for diagnostic output in failure messages.
func diffKeys(want map[string]string, got map[string]string) []string {
	var missing []string
	for k := range want {
		if _, ok := got[k]; !ok {
			missing = append(missing, k)
		}
	}
	return missing
}
