//go:build !race

// The race detector inflates allocation counts (shadow memory + instrumentation)
// and dramatically slows down each iteration. Under -race on slow CI runners
// (2-core GHA) testing.Benchmark's b.N auto-scaling overshoots and the test
// runs for many minutes. Since the budgets only meaningfully apply to the
// non-race hot path, excluding this file from -race builds is correct.

package memcached

import (
	"context"
	"os"
	"testing"
)

// TestAllocBudgets reports the current allocations-per-op for memcached
// operations the v1.4.0 profile-driven optimization loop targets.
//
// Behaviour:
//   - Default: always logs `allocs/op=X (budget N)` so every test run surfaces
//     current allocation profiles, but never fails. Allocation regressions
//     need to be fixed by the optimization pass, not block merges.
//   - With TESTING_STRICT_ALLOC_BUDGETS=1: the test fails if the observed
//     allocations exceed the budget.
//
// Benchmarks use the in-process fakeMemcached harness so these tests are
// hermetic — no live memcached server required.
//
// The fake server runs on a goroutine in the same process, so its
// per-command allocations (bufio.Reader framing, command arg slices,
// response writers) are attributed to the benchmark. These budgets
// therefore measure client-side + fake-side allocations together, and the
// budget values reflect the cost of the full in-process round trip. The
// driver's CLIENT-ONLY allocations on a real memcached server are lower:
//
//	Path            fake (this test)   real memcached (external bench)
//	GET small       4 allocs           2 allocs
//	SET small       4 allocs           2 allocs
//	GetMulti(10)    26 allocs          24 allocs
//	PoolAcquire     0                  0
//
// The real-memcached numbers are captured in the drivercmp suite under
// test/drivercmp/memcached when CELERIS_MEMCACHED_ADDR points at a live
// server. Strict mode here gates on the fake-path totals, which is the
// right regression surface for the driver code itself.
func TestAllocBudgets(t *testing.T) {
	strict := os.Getenv("TESTING_STRICT_ALLOC_BUDGETS") == "1"

	type guard struct {
		name   string
		budget int64
		bench  func(b *testing.B)
	}

	guards := []guard{
		// GET/SET: fake path is 4 allocs — 2 on the driver + 2 in the
		// fake's command parser/writer. Matches the v1.4.0 alloc-budget
		// target once you account for the in-process harness.
		{name: "GET_small", budget: 4, bench: benchGETSmall},
		{name: "SET_small", budget: 4, bench: benchSETSmall},
		// GetMulti(10): driver copyBytes(Data) × 10 (10 allocs) +
		// string(Key) × 10 (10 allocs) + output map + bridge overhead +
		// fake response construction. Improving further requires a
		// per-request slab allocation that replaces the per-value copies
		// — tracked for v1.4.1.
		{name: "GetMulti_10", budget: 26, bench: benchGetMulti10},
		{name: "PoolAcquireRelease", budget: 0, bench: benchPoolAcquireRelease},
	}

	for _, g := range guards {
		t.Run(g.name, func(t *testing.T) {
			res := testing.Benchmark(g.bench)
			got := res.AllocsPerOp()
			t.Logf("allocs/op=%d (budget %d) — %s", got, g.budget, res)
			if strict && got > g.budget {
				t.Fatalf("allocs/op %d exceeds budget %d (strict mode)", got, g.budget)
			}
		})
	}
}

func benchGETSmall(b *testing.B) {
	fake := startFakeB(b)
	fake.store.kv["k"] = memItem{value: []byte("v"), cas: 1}
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	// Warm the pool.
	if _, err := c.Get(ctx, "k"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Get(ctx, "k"); err != nil {
			b.Fatal(err)
		}
	}
}

func benchSETSmall(b *testing.B) {
	fake := startFakeB(b)
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	if err := c.Set(ctx, "k", "v", 0); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Set(ctx, "k", "v", 0); err != nil {
			b.Fatal(err)
		}
	}
}

func benchGetMulti10(b *testing.B) {
	fake := startFakeB(b)
	keys := []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9"}
	for _, k := range keys {
		fake.store.kv[k] = memItem{value: []byte("v"), cas: uint64(len(fake.store.kv)) + 1}
	}
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	// Warm.
	if _, err := c.GetMulti(ctx, keys...); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.GetMulti(ctx, keys...); err != nil {
			b.Fatal(err)
		}
	}
}

func benchPoolAcquireRelease(b *testing.B) {
	fake := startFakeB(b)
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	// Warm.
	if _, err := c.pool.acquire(ctx, -1); err != nil {
		b.Fatal(err)
	}
	conn, _ := c.pool.acquire(ctx, -1)
	c.pool.release(conn)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		conn, err := c.pool.acquire(ctx, -1)
		if err != nil {
			b.Fatal(err)
		}
		c.pool.release(conn)
	}
}
