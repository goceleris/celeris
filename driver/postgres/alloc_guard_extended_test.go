//go:build !race

// Under -race, testing.Benchmark's b.N auto-scaling overshoots on slow CI
// runners and the test runs for many minutes before timing out. The budget
// only meaningfully applies to the non-race hot path.

package postgres

import (
	"context"
	"database/sql/driver"
	"net"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/internal/eventloop"
	"github.com/goceleris/celeris/driver/postgres/protocol"
)

// benchPreparedExec measures allocs/op for a prepared-statement Exec against
// the extended-protocol fake. We Prepare once outside the loop so the
// measurement only includes the Bind + Execute + Sync round trip.
func benchPreparedExec(b *testing.B) {
	addr := startFakePGBench(b, extendedFakeServer(b, []uint32{protocol.OIDInt8, protocol.OIDText}, "INSERT 0 1"))
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		b.Fatal(err)
	}
	defer eventloop.Release(prov)
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{Host: host, Port: port, User: "u", Options: Options{SSLMode: "disable", StatementCacheSize: 16}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	stmt, err := c.PrepareContext(ctx, "INSERT INTO t(id, name) VALUES($1, $2)")
	if err != nil {
		b.Fatal(err)
	}
	ps := stmt.(*pgStmt)

	args := []driver.NamedValue{
		{Ordinal: 1, Value: int64(1)},
		{Ordinal: 2, Value: "x"},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ps.ExecContext(ctx, args); err != nil {
			b.Fatal(err)
		}
	}
}

// TestAllocBudgetsPreparedExec reports allocations-per-op for a prepared
// INSERT-style Exec against the extended-protocol fake harness. The test
// behaves like TestAllocBudgets: default mode logs + does not fail;
// TESTING_STRICT_ALLOC_BUDGETS=1 enforces the budget.
func TestAllocBudgetsPreparedExec(t *testing.T) {
	strict := os.Getenv("TESTING_STRICT_ALLOC_BUDGETS") == "1"
	const budget int64 = 32 // aspirational — tightened later by the opt loop.

	res := testing.Benchmark(benchPreparedExec)
	got := res.AllocsPerOp()
	t.Logf("PreparedExec allocs/op=%d (budget %d) — %s", got, budget, res)
	if strict && got > budget {
		t.Fatalf("allocs/op %d exceeds budget %d (strict mode)", got, budget)
	}
}
