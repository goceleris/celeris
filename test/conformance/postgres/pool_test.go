//go:build postgres

package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/driver/postgres"
)

// TestPoolBasic opens a Pool, pings, runs a Query, checks stats.
func TestPoolBasic(t *testing.T) {
	dsn := dsnFromEnv(t)
	p, err := postgres.Open(dsn,
		postgres.WithMaxOpen(4),
		postgres.WithMaxIdlePerWorker(1),
		postgres.WithMaxLifetime(5*time.Minute),
	)
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	rows, err := p.QueryContext(ctx, "SELECT 1::int, 'hi'::text")
	if err != nil {
		t.Fatalf("QueryContext: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("expected a row")
	}
	var id int64
	var name string
	if err := rows.Scan(&id, &name); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	stats := p.Stats()
	_ = stats // Just make sure it returns without panic — fields vary.
}

// TestPoolWorkerAffinity pins one context to worker 0 and another to worker 1
// and makes sure both succeed. The hint is advisory; we don't assert which
// worker actually ran the callback.
func TestPoolWorkerAffinity(t *testing.T) {
	dsn := dsnFromEnv(t)
	p, err := postgres.Open(dsn, postgres.WithMaxOpen(8), postgres.WithMaxIdlePerWorker(2))
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	ctx0 := postgres.WithWorker(context.Background(), 0)
	ctx1 := postgres.WithWorker(context.Background(), 1)

	for _, ctx := range []context.Context{ctx0, ctx1} {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := p.Ping(c); err != nil {
			cancel()
			t.Fatalf("ping worker-hinted ctx: %v", err)
		}
		cancel()
	}
}

// TestPoolContention fires many concurrent Ping calls against a small pool to
// ensure the semaphore + idle-list logic doesn't deadlock.
func TestPoolContention(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping contention test in -short mode")
	}
	dsn := dsnFromEnv(t)
	p, err := postgres.Open(dsn, postgres.WithMaxOpen(4))
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := p.Ping(ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Ping: %v", err)
	}
}
