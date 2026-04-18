package memcached

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// startFakeCluster spins up n in-process fake memcached servers and returns
// both the fakes and their addresses. Each fake has its own independent
// memStore; writes routed through the ring land on exactly one fake, which
// mirrors real cluster behavior.
func startFakeCluster(t *testing.T, n int) (fakes []*fakeMemcached, addrs []string) {
	t.Helper()
	fakes = make([]*fakeMemcached, n)
	addrs = make([]string, n)
	for i := 0; i < n; i++ {
		fakes[i] = startFake(t)
		addrs[i] = fakes[i].Addr()
	}
	return fakes, addrs
}

// ringDistribution counts ring owners for n random keys and returns a
// count-per-node map keyed by address.
func ringDistribution(cc *ClusterClient, n int) map[string]int {
	dist := make(map[string]int, len(cc.nodes))
	rng := rand.New(rand.NewPCG(0xC0FFEE, 0xF00D))
	for i := 0; i < n; i++ {
		// 16-byte random-looking keys cover the CRC32 space well.
		key := strconv.FormatUint(rng.Uint64(), 36) + "-" + strconv.FormatUint(rng.Uint64(), 36)
		addr := cc.NodeFor(key)
		dist[addr]++
	}
	return dist
}

func TestClusterRingKetamaDistribution(t *testing.T) {
	_, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       addrs,
		DialTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	const total = 10_000
	dist := ringDistribution(cc, total)
	if len(dist) != 3 {
		t.Fatalf("want 3 owners, got %d: %v", len(dist), dist)
	}
	expected := total / 3
	// ±10% tolerance per node — generous for 160 vnodes/weight and
	// 10k samples. libmemcached publishes ~33±3% typical spread.
	tolerance := total / 10
	for addr, count := range dist {
		if count < expected-tolerance || count > expected+tolerance {
			t.Errorf("node %s owns %d keys (%.2f%%), want ~%d (±%d)",
				addr, count, 100*float64(count)/float64(total), expected, tolerance)
		}
	}
}

func TestClusterRingWeighted(t *testing.T) {
	_, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{
		Addrs:       addrs,
		Weights:     []uint32{1, 2, 1},
		DialTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	const total = 10_000
	dist := ringDistribution(cc, total)
	heavy := dist[addrs[1]]
	// weight-2 node should own ~50% of the ring (2 / (1+2+1)).
	expected := total / 2
	tolerance := total / 10
	if heavy < expected-tolerance || heavy > expected+tolerance {
		t.Errorf("heavy node owns %d keys (%.2f%%), want ~%d (±%d)",
			heavy, 100*float64(heavy)/float64(total), expected, tolerance)
	}
	// And the two weight-1 nodes together should own the remaining ~50%.
	light := dist[addrs[0]] + dist[addrs[2]]
	if light < expected-tolerance || light > expected+tolerance {
		t.Errorf("light nodes own %d keys total, want ~%d (±%d)", light, expected, tolerance)
	}
}

func TestClusterNodeForStable(t *testing.T) {
	_, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	for i := 0; i < 200; i++ {
		k := fmt.Sprintf("stable-%d", i)
		first := cc.NodeFor(k)
		for j := 0; j < 5; j++ {
			if got := cc.NodeFor(k); got != first {
				t.Fatalf("NodeFor(%q) unstable: %q vs %q on iter %d", k, first, got, j)
			}
		}
	}
}

func TestClusterNodeForConsistentOnFailure(t *testing.T) {
	// Build a 3-node ring, record the owner of 5000 keys, drop the middle
	// node, rebuild, and assert most keys kept their previous node.
	_, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	const total = 5_000
	rng := rand.New(rand.NewPCG(0xBEEF, 0xDEAD))
	keys := make([]string, total)
	before := make(map[string]string, total)
	for i := 0; i < total; i++ {
		keys[i] = strconv.FormatUint(rng.Uint64(), 36) + "/" + strconv.Itoa(i)
		before[keys[i]] = cc.NodeFor(keys[i])
	}

	// Drop the middle node (addrs[1]).
	survivingAddrs := []string{addrs[0], addrs[2]}
	cc2, err := NewClusterClient(ClusterConfig{Addrs: survivingAddrs, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClusterClient 2-node: %v", err)
	}
	defer func() { _ = cc2.Close() }()

	kept := 0
	for _, k := range keys {
		if before[k] == addrs[1] {
			// These keys *must* move — their old owner is gone.
			continue
		}
		if cc2.NodeFor(k) == before[k] {
			kept++
		}
	}
	// Keys that were not on the removed node should mostly keep their owner.
	// Ketama with 160 vnodes guarantees that removing one of N nodes only
	// re-homes the range owned by that node (~1/N), not the whole ring.
	// Non-lost keys ≈ 2/3 of total; of those, essentially all should stick.
	nonLost := 0
	for _, k := range keys {
		if before[k] != addrs[1] {
			nonLost++
		}
	}
	// Strict ketama: all non-lost keys should retain their owner. Require
	// at least 95% as a robust floor (still well above the >=50% bar in
	// the task spec, which accounts for hash-collision edge cases).
	floor := nonLost * 95 / 100
	if kept < floor {
		t.Fatalf("only %d/%d non-lost keys kept their owner (%.1f%%); want >=%d",
			kept, nonLost, 100*float64(kept)/float64(nonLost), floor)
	}

	// And a >=50% of the total (including lost) also passes the task's
	// documented floor, just to be explicit.
	if kept < total/2 {
		t.Fatalf("kept %d/%d keys, want >=%d", kept, total, total/2)
	}
}

func TestClusterSetGetRoundTrip(t *testing.T) {
	_, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	for i := 0; i < 20; i++ {
		k := fmt.Sprintf("round-%d", i)
		v := fmt.Sprintf("val-%d", i)
		if err := cc.Set(ctx, k, v, 0); err != nil {
			t.Fatalf("Set(%s): %v", k, err)
		}
	}
	for i := 0; i < 20; i++ {
		k := fmt.Sprintf("round-%d", i)
		got, err := cc.Get(ctx, k)
		if err != nil {
			t.Fatalf("Get(%s): %v", k, err)
		}
		if want := fmt.Sprintf("val-%d", i); got != want {
			t.Fatalf("Get(%s) = %q, want %q", k, got, want)
		}
	}
}

func TestClusterGetMultiFanOut(t *testing.T) {
	fakes, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	const total = 30 // spread enough to hit every node
	keys := make([]string, total)
	for i := 0; i < total; i++ {
		keys[i] = fmt.Sprintf("fanout-%d", i)
		if err := cc.Set(ctx, keys[i], fmt.Sprintf("v%d", i), 0); err != nil {
			t.Fatalf("Set(%s): %v", keys[i], err)
		}
	}
	// Baseline the per-fake key-load counters *after* the sets so we only
	// measure the GetMulti traffic below.
	var baseGetKeys [3]uint64
	for i, f := range fakes {
		baseGetKeys[i] = f.getKeys.Load()
	}

	got, err := cc.GetMulti(ctx, keys...)
	if err != nil {
		t.Fatalf("GetMulti: %v", err)
	}
	if len(got) != total {
		t.Fatalf("GetMulti returned %d keys, want %d", len(got), total)
	}
	for i, k := range keys {
		want := fmt.Sprintf("v%d", i)
		if got[k] != want {
			t.Errorf("GetMulti[%s] = %q, want %q", k, got[k], want)
		}
	}

	// Assert every fake saw *some* share of the traffic — i.e. the fan-out
	// actually hit more than one server. Exact proportions depend on the
	// ring's random layout but with 30 keys across 3 nodes each node
	// should have seen at least one key.
	for i, f := range fakes {
		delta := f.getKeys.Load() - baseGetKeys[i]
		if delta == 0 {
			t.Errorf("fake %d saw 0 GET keys; expected fan-out across all nodes", i)
		}
	}
	// And the total keys served should equal len(keys) exactly (each key
	// routes to exactly one node).
	var total2 uint64
	for i, f := range fakes {
		total2 += f.getKeys.Load() - baseGetKeys[i]
	}
	if total2 != uint64(total) {
		t.Errorf("total GET keys across fakes = %d, want %d", total2, total)
	}
}

func TestClusterGetMultiBytes(t *testing.T) {
	_, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	keys := []string{"a", "b", "c", "d", "e"}
	for _, k := range keys {
		if err := cc.Set(ctx, k, []byte("v-"+k), 0); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	got, err := cc.GetMultiBytes(ctx, keys...)
	if err != nil {
		t.Fatalf("GetMultiBytes: %v", err)
	}
	for _, k := range keys {
		if string(got[k]) != "v-"+k {
			t.Errorf("GetMultiBytes[%s] = %q", k, got[k])
		}
	}
}

func TestClusterStats(t *testing.T) {
	_, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	stats, err := cc.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats) != 3 {
		t.Fatalf("Stats returned %d node entries, want 3", len(stats))
	}
	// Keys should mirror the configured addresses.
	gotAddrs := make([]string, 0, len(stats))
	for a := range stats {
		gotAddrs = append(gotAddrs, a)
	}
	sort.Strings(gotAddrs)
	wantAddrs := append([]string(nil), addrs...)
	sort.Strings(wantAddrs)
	for i := range wantAddrs {
		if gotAddrs[i] != wantAddrs[i] {
			t.Errorf("Stats node %d: got %q, want %q", i, gotAddrs[i], wantAddrs[i])
		}
	}
	for a, s := range stats {
		if s["version"] == "" && s["pid"] == "" {
			t.Errorf("Stats[%s] missing expected fields: %v", a, s)
		}
	}
}

func TestClusterVersion(t *testing.T) {
	_, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	vers, err := cc.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if len(vers) != 3 {
		t.Fatalf("Version returned %d entries, want 3", len(vers))
	}
	for a, v := range vers {
		if v == "" {
			t.Errorf("Version[%s] empty", a)
		}
	}
}

func TestClusterFlushFansOut(t *testing.T) {
	fakes, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	ctx := context.Background()
	// Plant one key on every node so we can assert flush reached each.
	for _, f := range fakes {
		f.store.mu.Lock()
		f.store.kv["planted"] = memItem{value: []byte("seed"), cas: f.store.nextCAS()}
		f.store.mu.Unlock()
	}
	if err := cc.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	for i, f := range fakes {
		f.store.mu.Lock()
		n := len(f.store.kv)
		f.store.mu.Unlock()
		if n != 0 {
			t.Errorf("fake %d still has %d items after Flush", i, n)
		}
	}
}

func TestClusterPing(t *testing.T) {
	_, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()
	if err := cc.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestClusterClosedReturnsErr(t *testing.T) {
	_, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	if err := cc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx := context.Background()
	if err := cc.Set(ctx, "k", "v", 0); !errors.Is(err, ErrClosed) {
		t.Errorf("Set after Close: want ErrClosed, got %v", err)
	}
	if _, err := cc.Get(ctx, "k"); !errors.Is(err, ErrClosed) {
		t.Errorf("Get after Close: want ErrClosed, got %v", err)
	}
	if _, err := cc.GetBytes(ctx, "k"); !errors.Is(err, ErrClosed) {
		t.Errorf("GetBytes after Close: want ErrClosed, got %v", err)
	}
	if _, err := cc.GetMulti(ctx, "k"); !errors.Is(err, ErrClosed) {
		t.Errorf("GetMulti after Close: want ErrClosed, got %v", err)
	}
	if _, err := cc.GetMultiBytes(ctx, "k"); !errors.Is(err, ErrClosed) {
		t.Errorf("GetMultiBytes after Close: want ErrClosed, got %v", err)
	}
	if err := cc.Delete(ctx, "k"); !errors.Is(err, ErrClosed) {
		t.Errorf("Delete after Close: want ErrClosed, got %v", err)
	}
	if err := cc.Flush(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Flush after Close: want ErrClosed, got %v", err)
	}
	if _, err := cc.Stats(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Stats after Close: want ErrClosed, got %v", err)
	}
	if _, err := cc.Version(ctx); !errors.Is(err, ErrClosed) {
		t.Errorf("Version after Close: want ErrClosed, got %v", err)
	}

	// Double close must be idempotent.
	if err := cc.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestClusterValidatesConfig(t *testing.T) {
	// Empty Addrs.
	if _, err := NewClusterClient(ClusterConfig{}); err == nil {
		t.Fatal("expected error for empty Addrs")
	}
	// Weights length mismatch.
	_, addrs := startFakeCluster(t, 2)
	if _, err := NewClusterClient(ClusterConfig{
		Addrs:   addrs,
		Weights: []uint32{1},
	}); err == nil {
		t.Fatal("expected error for weights length mismatch")
	}
	// Zero weight.
	if _, err := NewClusterClient(ClusterConfig{
		Addrs:   addrs,
		Weights: []uint32{1, 0},
	}); err == nil {
		t.Fatal("expected error for zero weight")
	}
}

func TestClusterAddrsAndNodeFor(t *testing.T) {
	_, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	got := cc.Addrs()
	if len(got) != len(addrs) {
		t.Fatalf("Addrs len %d, want %d", len(got), len(addrs))
	}
	for i := range addrs {
		if got[i] != addrs[i] {
			t.Errorf("Addrs[%d] = %q, want %q", i, got[i], addrs[i])
		}
	}
	// NodeFor must return one of the configured addrs.
	seen := map[string]bool{}
	for _, a := range addrs {
		seen[a] = false
	}
	for i := 0; i < 500; i++ {
		a := cc.NodeFor(fmt.Sprintf("k-%d", i))
		if _, ok := seen[a]; !ok {
			t.Fatalf("NodeFor returned unknown addr %q", a)
		}
		seen[a] = true
	}
	for a, hit := range seen {
		if !hit {
			t.Errorf("addr %s never selected across 500 random keys", a)
		}
	}
}

func TestClusterConcurrentSetGet(t *testing.T) {
	_, addrs := startFakeCluster(t, 3)
	cc, err := NewClusterClient(ClusterConfig{Addrs: addrs, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("NewClusterClient: %v", err)
	}
	defer func() { _ = cc.Close() }()

	const nGoroutines = 16
	const nKeysEach = 50
	ctx := context.Background()

	var wg sync.WaitGroup
	for g := 0; g < nGoroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < nKeysEach; i++ {
				k := fmt.Sprintf("c-%d-%d", g, i)
				v := fmt.Sprintf("v-%d-%d", g, i)
				if err := cc.Set(ctx, k, v, 0); err != nil {
					t.Errorf("Set: %v", err)
					return
				}
				got, err := cc.Get(ctx, k)
				if err != nil {
					t.Errorf("Get: %v", err)
					return
				}
				if got != v {
					t.Errorf("Get = %q, want %q", got, v)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
