//go:build !race

// The race detector inflates allocation counts (shadow memory + instrumentation)
// and dramatically slows down each iteration. Under -race on slow CI runners
// (2-core GHA) testing.Benchmark's b.N auto-scaling overshoots and the test
// runs for many minutes. Since the budgets only meaningfully apply to the
// non-race hot path, excluding this file from -race builds is correct.

package redis

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
)

// TestAllocBudgets reports the current allocations-per-op for the small set
// of Redis operations that the v1.4.0 profile-driven optimization loop is
// targeting (plan §6.6). Each sub-test runs the relevant benchmark once via
// testing.Benchmark and compares its AllocsPerOp against an aspirational
// budget.
//
// Behaviour:
//   - Default: always logs `allocs/op=X (budget N)` so every test run
//     surfaces current allocation profiles, but never fails. Allocation
//     regressions need to be fixed by the optimization pass, not block
//     merges.
//   - With TESTING_STRICT_ALLOC_BUDGETS=1: the test fails if the observed
//     allocations exceed the budget. Useful for CI jobs that explicitly
//     want the gate enforced once the loop has landed its improvements.
//
// Benchmarks use the in-process fakeRedis + memStore harness so these
// tests are hermetic — no live Redis required.
func TestAllocBudgets(t *testing.T) {
	strict := os.Getenv("TESTING_STRICT_ALLOC_BUDGETS") == "1"

	type guard struct {
		name   string
		budget int64
		// bench is a function that takes a *testing.B and runs b.N
		// iterations of the op under test. It's responsible for all
		// setup (client, fake server) and must call b.ResetTimer and
		// b.ReportAllocs.
		bench func(b *testing.B)
	}

	guards := []guard{
		{
			// GET allocs breakdown (steady state):
			//   1 client-side:  copyValueDetached makes a fresh []byte for
			//                   v.Str so the reply survives the reader's next
			//                   Feed/Compact cycle (worker goroutine).
			//   1 client-side:  string(v.Str) inside asString — the
			//                   user-visible result is a Go string; we do not
			//                   expose the aliased buffer.
			// Zero would require unsafe.String + external lifetime
			// management, which we refuse.
			name:   "GET_small",
			budget: 2,
			bench:  benchGETSmall,
		},
		{
			// SET allocs breakdown (steady state):
			//   1 client-side:  copyValueDetached copies the "OK" simple
			//                   string (even though the caller discards it —
			//                   the dispatch path is uniform).
			//   1 test-harness: fake-server side, per-cmd args []string slice.
			// The client itself contributes 1.
			name:   "SET_small",
			budget: 2,
			bench:  benchSETSmall,
		},
		{
			name:   "MGET_10",
			budget: 25,
			bench:  benchMGET10,
		},
		{
			// Pipeline(100) allocs breakdown (steady state, Release called):
			//   ~0 client-side: Pipeline + all per-cmd tracking structs
			//                   (pipeCmd, IntCmd, doneCh-equivalent, reqs
			//                   slice, writer buffer) are pooled on the
			//                   Client; Engine.Write() copies without a
			//                   client-side allocation.
			//   100 test-harness: the fake memStore stringifies the new
			//                     counter (itoa) on every INCR reply — one
			//                     alloc per command. Eliminating this
			//                     requires restructuring the memStore, not
			//                     the driver.
			// Pragmatic floor for this test harness; anything > 100 indicates
			// a client-side regression.
			name:   "Pipeline_100",
			budget: 100,
			bench:  benchPipeline100,
		},
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
	mem := newMem()
	mem.kv["k"] = "v"
	fake := startFakeRedisBench(b, mem.handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Get(ctx, "k"); err != nil {
			b.Fatal(err)
		}
	}
}

func benchSETSmall(b *testing.B) {
	mem := newMem()
	fake := startFakeRedisBench(b, mem.handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Set(ctx, "k", "v", 0); err != nil {
			b.Fatal(err)
		}
	}
}

func benchMGET10(b *testing.B) {
	// The in-package memStore test harness doesn't script MGET; wrap its
	// handler so MGET returns an array of bulks for the requested keys.
	mem := newMem()
	for _, k := range []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9"} {
		mem.kv[k] = "v"
	}
	handler := func(cmd []string, w *bufio.Writer) {
		if len(cmd) > 0 && strings.EqualFold(cmd[0], "MGET") {
			mem.mu.Lock()
			writeArrayHeader(w, len(cmd)-1)
			for _, k := range cmd[1:] {
				if v, ok := mem.kv[k]; ok {
					writeBulk(w, v)
				} else {
					writeNullBulk(w)
				}
			}
			mem.mu.Unlock()
			return
		}
		mem.handler(cmd, w)
	}
	fake := startFakeRedisBench(b, handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	keys := []string{"k0", "k1", "k2", "k3", "k4", "k5", "k6", "k7", "k8", "k9"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.MGet(ctx, keys...); err != nil {
			b.Fatal(err)
		}
	}
}

func benchPipeline100(b *testing.B) {
	mem := newMem()
	fake := startFakeRedisBench(b, mem.handler)
	c, err := NewClient(fake.Addr())
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := c.Pipeline()
		for j := 0; j < 100; j++ {
			p.Incr("ctr")
		}
		if err := p.Exec(ctx); err != nil {
			b.Fatal(err)
		}
		// Return the Pipeline to its pool so the slab backing is reused
		// across iterations — steady-state allocations are what the
		// budget is tracking.
		p.Release()
	}
}
