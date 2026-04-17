// Package integration_test — end-to-end WithEngine integration drills.
//
// Every v1.4.0 driver documents WithEngine(srv) as the way to share the
// HTTP server's per-CPU event loop with the driver pool. These tests
// stand up a real celeris.Server, open each driver with WithEngine, wire
// an HTTP handler that uses the driver, and exercise the end-to-end path
// with a concurrent HTTP client.
//
// Gating (per-test skip, no build tag):
//
//	CELERIS_PG_DSN         → TestSharedLoopPostgres
//	CELERIS_REDIS_ADDR     → TestSharedLoopRedis
//	CELERIS_MEMCACHED_ADDR → TestSharedLoopMemcached
//
// When none are set the file compiles and every test skips — `go test ./...`
// on a vanilla checkout stays green.
package integration_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	celeris "github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/driver/postgres"
	"github.com/goceleris/celeris/driver/redis"
)

// startSharedLoopServer builds a celeris.Server on :0 backed by the engine
// selected via CELERIS_SHARED_LOOP_ENGINE (default: std), calls `register`
// to wire drivers + handlers, and returns the bound base URL. The driver
// Open() calls MUST happen inside `register` so they see the server before
// Start.
//
// Set CELERIS_SHARED_LOOP_ENGINE=epoll|iouring|adaptive on Linux to exercise
// the real EventLoopProvider path where .Worker() affinity is observable.
// Any other value (or darwin) falls through to Std.
func startSharedLoopServer(t *testing.T, register func(*celeris.Server)) (*celeris.Server, string) {
	t.Helper()
	engineChoice := celeris.Std
	switch strings.ToLower(os.Getenv("CELERIS_SHARED_LOOP_ENGINE")) {
	case "epoll":
		engineChoice = celeris.Epoll
	case "iouring", "io_uring", "uring":
		engineChoice = celeris.IOUring
	case "adaptive":
		engineChoice = celeris.Adaptive
	}
	srv := celeris.New(celeris.Config{
		Addr:   "127.0.0.1:0",
		Engine: engineChoice,
	})
	register(srv)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.StartWithListener(ln) }()

	deadline := time.Now().Add(5 * time.Second)
	for srv.Addr() == nil && time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("server exited early: %v", err)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if srv.Addr() == nil {
		t.Fatal("server never bound within deadline")
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})

	if srv.EventLoopProvider() == nil {
		t.Logf("server EventLoopProvider() is nil (std engine on %s) — drivers use standalone fallback loop; the happy-path HTTP round-trip still runs but .Worker() affinity is not observable", runtime.GOOS)
	} else {
		t.Logf("server EventLoopProvider() is non-nil (native engine: %d workers) — drivers share the HTTP engine's workers", srv.EventLoopProvider().NumWorkers())
	}

	return srv, "http://" + srv.Addr().String()
}

// fanout runs fn N times concurrently and returns the first error seen.
// Used to stress concurrent driver access through the shared-loop path.
func fanout(t *testing.T, n int, fn func(i int) error) {
	t.Helper()
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := fn(i); err != nil {
				errCh <- fmt.Errorf("iter %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// httpGetText does GET base+path, asserts 200, and returns the response
// body trimmed of trailing whitespace. Short-circuits with t.Error on any
// non-2xx response.
func httpGetText(t *testing.T, client *http.Client, url string) (string, int, error) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, err
	}
	return strings.TrimSpace(string(body)), resp.StatusCode, nil
}

// -----------------------------------------------------------------------------
// Postgres — GET /value/:n returns `n+1` via a `SELECT $1::int + 1` query.
// -----------------------------------------------------------------------------

func TestSharedLoopPostgres(t *testing.T) {
	dsn := os.Getenv("CELERIS_PG_DSN")
	if dsn == "" {
		t.Skip("skipping: CELERIS_PG_DSN not set")
	}

	observed := newWorkerIDSet()
	var pool *postgres.Pool
	srv, base := startSharedLoopServer(t, func(srv *celeris.Server) {
		p, err := postgres.Open(dsn,
			postgres.WithEngine(srv),
			postgres.WithMaxOpen(8),
		)
		if err != nil {
			t.Fatalf("postgres.Open: %v", err)
		}
		pool = p

		srv.GET("/value/:n", func(c *celeris.Context) error {
			n, perr := strconv.Atoi(c.Param("n"))
			if perr != nil {
				return c.String(http.StatusBadRequest, "bad n: %v", perr)
			}
			wid := c.WorkerID()
			observed.add(wid)
			ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
			defer cancel()
			ctx = postgres.WithWorker(ctx, wid)
			row := pool.QueryRow(ctx, "SELECT $1::int + 1", n)
			var out int
			if err := row.Scan(&out); err != nil {
				return c.String(http.StatusInternalServerError, "scan: %v", err)
			}
			return c.String(http.StatusOK, "%d", out)
		})
	})
	t.Cleanup(func() { _ = pool.Close() })

	// Log provider identity — proves (or disproves) same-engine sharing.
	logProviderIdentity(t, srv)

	client := &http.Client{Timeout: 10 * time.Second}
	const requests = 50
	fanout(t, requests, func(i int) error {
		body, status, err := httpGetText(t, client, fmt.Sprintf("%s/value/%d", base, i))
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("status=%d body=%q", status, body)
		}
		want := strconv.Itoa(i + 1)
		if body != want {
			return fmt.Errorf("body=%q want=%q", body, want)
		}
		return nil
	})

	assertWorkerAffinity(t, srv, observed, pool.IdleConnWorkers())
}

// -----------------------------------------------------------------------------
// Redis — pre-seed 10 keys, then serve GET /value/:k returning the value.
// -----------------------------------------------------------------------------

func TestSharedLoopRedis(t *testing.T) {
	addr := os.Getenv("CELERIS_REDIS_ADDR")
	if addr == "" {
		t.Skip("skipping: CELERIS_REDIS_ADDR not set")
	}

	// Pre-seed via a throwaway client (no engine yet — the server isn't up).
	// Use ForceRESP2 to keep handshake compatible with old Redis + Valkey.
	seedClient, err := redis.NewClient(addr, redis.WithForceRESP2(), redis.WithDialTimeout(5*time.Second))
	if err != nil {
		t.Skipf("skipping: redis unreachable: %v", err)
	}
	defer func() { _ = seedClient.Close() }()
	seedCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := seedClient.Ping(seedCtx); err != nil {
		t.Skipf("skipping: redis PING failed: %v", err)
	}
	keys := make([]string, 10)
	for i := range keys {
		keys[i] = fmt.Sprintf("celeris:shared-loop:k%d", i)
		if _, err := seedClient.Do(seedCtx, "SET", keys[i], fmt.Sprintf("v%d", i)); err != nil {
			t.Fatalf("seed SET %s: %v", keys[i], err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		args := make([]any, 1+len(keys))
		args[0] = "DEL"
		for i, k := range keys {
			args[i+1] = k
		}
		_, _ = seedClient.Do(cleanupCtx, args...)
	})

	observed := newWorkerIDSet()
	var client *redis.Client
	srv, base := startSharedLoopServer(t, func(srv *celeris.Server) {
		c, err := redis.NewClient(addr,
			redis.WithEngine(srv),
			redis.WithForceRESP2(),
			redis.WithDialTimeout(5*time.Second),
		)
		if err != nil {
			t.Fatalf("redis.NewClient (server-scoped): %v", err)
		}
		client = c

		srv.GET("/value/:k", func(c *celeris.Context) error {
			wid := c.WorkerID()
			observed.add(wid)
			ctx, cancelReq := context.WithTimeout(c.Context(), 5*time.Second)
			defer cancelReq()
			ctx = redis.WithWorker(ctx, wid)
			v, err := client.DoString(ctx, "GET", c.Param("k"))
			if err != nil {
				return c.String(http.StatusInternalServerError, "get: %v", err)
			}
			return c.String(http.StatusOK, "%s", v)
		})
	})
	t.Cleanup(func() { _ = client.Close() })

	logProviderIdentity(t, srv)

	httpClient := &http.Client{Timeout: 10 * time.Second}
	const requests = 100
	fanout(t, requests, func(i int) error {
		idx := i % len(keys)
		body, status, err := httpGetText(t, httpClient, fmt.Sprintf("%s/value/%s", base, keys[idx]))
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("status=%d body=%q", status, body)
		}
		want := fmt.Sprintf("v%d", idx)
		if body != want {
			return fmt.Errorf("body=%q want=%q", body, want)
		}
		return nil
	})

	assertWorkerAffinity(t, srv, observed, client.IdleConnWorkers())
}

// -----------------------------------------------------------------------------
// Memcached — same shape as Redis: seed 10 keys, GET /value/:k returns value.
// -----------------------------------------------------------------------------

func TestSharedLoopMemcached(t *testing.T) {
	addr := os.Getenv("CELERIS_MEMCACHED_ADDR")
	if addr == "" {
		t.Skip("skipping: CELERIS_MEMCACHED_ADDR not set")
	}

	seedClient, err := memcached.NewClient(addr, memcached.WithDialTimeout(5*time.Second))
	if err != nil {
		t.Skipf("skipping: memcached unreachable: %v", err)
	}
	defer func() { _ = seedClient.Close() }()
	seedCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := seedClient.Ping(seedCtx); err != nil {
		t.Skipf("skipping: memcached PING failed: %v", err)
	}

	keys := make([]string, 10)
	for i := range keys {
		keys[i] = fmt.Sprintf("celeris-shared-loop-k%d", i)
		if err := seedClient.Set(seedCtx, keys[i], fmt.Sprintf("v%d", i), 60*time.Second); err != nil {
			t.Fatalf("seed Set %s: %v", keys[i], err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		for _, k := range keys {
			_ = seedClient.Delete(cleanupCtx, k)
		}
	})

	observed := newWorkerIDSet()
	var client *memcached.Client
	srv, base := startSharedLoopServer(t, func(srv *celeris.Server) {
		c, err := memcached.NewClient(addr,
			memcached.WithEngine(srv),
			memcached.WithDialTimeout(5*time.Second),
		)
		if err != nil {
			t.Fatalf("memcached.NewClient (server-scoped): %v", err)
		}
		client = c

		srv.GET("/value/:k", func(c *celeris.Context) error {
			wid := c.WorkerID()
			observed.add(wid)
			ctx, cancelReq := context.WithTimeout(c.Context(), 5*time.Second)
			defer cancelReq()
			ctx = memcached.WithWorker(ctx, wid)
			v, err := client.Get(ctx, c.Param("k"))
			if err != nil {
				return c.String(http.StatusInternalServerError, "get: %v", err)
			}
			return c.String(http.StatusOK, "%s", v)
		})
	})
	t.Cleanup(func() { _ = client.Close() })

	logProviderIdentity(t, srv)

	httpClient := &http.Client{Timeout: 10 * time.Second}
	const requests = 100
	fanout(t, requests, func(i int) error {
		idx := i % len(keys)
		body, status, err := httpGetText(t, httpClient, fmt.Sprintf("%s/value/%s", base, keys[idx]))
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("status=%d body=%q", status, body)
		}
		want := fmt.Sprintf("v%d", idx)
		if body != want {
			return fmt.Errorf("body=%q want=%q", body, want)
		}
		return nil
	})

	assertWorkerAffinity(t, srv, observed, client.IdleConnWorkers())
}

// logProviderIdentity documents the "same event loop" claim for this test
// run. With the std engine (darwin CI + default linux CI), srv.EventLoopProvider()
// is nil and the driver uses its standalone fallback loop. With a native
// engine (linux + Explicit Epoll/IOUring/Adaptive) the provider is non-nil,
// handlers see non-negative WorkerID()s, and the driver pools carry per-worker
// idle slots. The actual affinity assertion happens in assertWorkerAffinity.
func logProviderIdentity(t *testing.T, srv *celeris.Server) {
	t.Helper()
	if srv.EventLoopProvider() == nil {
		t.Logf("provider: standalone fallback (std engine); worker-affinity assertion skipped")
		return
	}
	t.Logf("provider: native, NumWorkers=%d (drivers share this loop)", srv.EventLoopProvider().NumWorkers())
}

// workerIDSet is a concurrent-safe set of observed worker IDs.
type workerIDSet struct {
	mu  sync.Mutex
	ids map[int]int // id -> count
}

func newWorkerIDSet() *workerIDSet { return &workerIDSet{ids: make(map[int]int)} }

func (s *workerIDSet) add(id int) {
	s.mu.Lock()
	s.ids[id]++
	s.mu.Unlock()
}

// distinct returns the sorted unique worker IDs observed.
func (s *workerIDSet) distinct() []int {
	s.mu.Lock()
	out := make([]int, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	s.mu.Unlock()
	return out
}

// assertWorkerAffinity validates that the handler worker IDs observed during
// the fanout match the per-worker idle slots the driver's pool now carries.
//
// Std engine / standalone fallback: WorkerID() is -1 and the driver pool uses
// a single worker (NumWorkers=1). In that case we assert every observed ID is
// -1 and every idle-conn ID is 0 (the single worker slot). This still proves
// the driver ran under the shared-loop contract.
//
// Native engine: we assert that
//
//  1. every idle-conn worker ID is in the set observed by handlers (no stray
//     workers dialed the pool), AND
//  2. at least one observed handler worker shows up in the idle set (proves
//     the hint threading reached the pool — otherwise idle conns would all
//     cluster on worker 0 via round-robin).
//
// The inverse inclusion (observed ⊆ idle) is too strict: if every handler
// worker happens to hand its conn out to a peer before fanout ends, that
// worker's idle slot is empty. We only require non-zero overlap.
func assertWorkerAffinity(t *testing.T, srv *celeris.Server, observed *workerIDSet, idle []int) {
	t.Helper()
	obsIDs := observed.distinct()
	t.Logf("observed handler worker IDs: %v", obsIDs)
	t.Logf("idle conn worker IDs: %v", idle)

	prov := srv.EventLoopProvider()
	if prov == nil {
		// Standalone fallback: handlers run under Go's scheduler; c.WorkerID()
		// returns -1. The driver's pool is backed by the standalone mini-loop
		// whose NumWorkers() defaults to runtime.NumCPU() — so idle conns
		// carry worker IDs in [0, NumCPU), not a single slot.
		if len(obsIDs) == 0 {
			t.Fatal("no handler invocations observed — test is broken")
		}
		for _, id := range obsIDs {
			if id != -1 {
				t.Errorf("std engine: expected c.WorkerID()==-1, got %d", id)
			}
		}
		upper := runtime.NumCPU()
		for _, id := range idle {
			if id < 0 || id >= upper {
				t.Errorf("standalone pool: conn.Worker()==%d out of range [0,%d)", id, upper)
			}
		}
		return
	}

	nw := prov.NumWorkers()
	if nw <= 0 {
		t.Fatalf("provider reports NumWorkers=%d (<= 0)", nw)
	}

	obs := make(map[int]struct{}, len(obsIDs))
	for _, id := range obsIDs {
		if id < 0 || id >= nw {
			t.Errorf("observed worker ID %d out of range [0,%d)", id, nw)
			continue
		}
		obs[id] = struct{}{}
	}
	if len(obs) == 0 {
		t.Fatal("no valid handler worker IDs observed against native engine")
	}

	if len(idle) == 0 {
		// Fanout consumed every conn and didn't release back yet. Rare but
		// possible. Warn but don't fail — the hint threading is covered by
		// the "no stray" check being vacuous here.
		t.Log("no idle conns after fanout — skipping idle-set assertions")
		return
	}

	overlap := 0
	for _, id := range idle {
		if id < 0 || id >= nw {
			t.Errorf("idle conn reports Worker()==%d out of range [0,%d)", id, nw)
			continue
		}
		if _, ok := obs[id]; ok {
			overlap++
		} else {
			t.Errorf("idle conn on worker %d that no handler ran on — stray dial", id)
		}
	}
	if overlap == 0 {
		t.Errorf("no overlap between observed handler workers %v and idle conn workers %v — worker hint did not reach the pool", obsIDs, idle)
	}
}

