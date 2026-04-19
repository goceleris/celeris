package benchcmp_db

// Scenario B — Postgres single-row query.
//
// Setup: a small `bench_users` table with one row (id=1, name=alice).
// The bench drives GET /user requests; the server executes
// SELECT id, name FROM bench_users WHERE id=1 and responds with JSON.
//
// All frameworks use jackc/pgx/v5 except celeris which uses its
// native driver/postgres. This quantifies the end-to-end saving of
// running the DB driver on the same event loop as the HTTP engine.

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	celeris "github.com/goceleris/celeris"
	celpostgres "github.com/goceleris/celeris/driver/postgres"
	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
)

type userRow struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// primePGSchema creates (or resets) bench_users with one row.
// Used by each server startup so the first query succeeds.
func primePGSchema(dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS bench_users (id BIGINT PRIMARY KEY, name TEXT NOT NULL)`,
		`TRUNCATE bench_users`,
		`INSERT INTO bench_users (id, name) VALUES (1, 'alice')`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func skipIfNoPG(b *testing.B) string {
	b.Helper()
	dsn := livePGDSN()
	if err := primePGSchema(dsn); err != nil {
		b.Skipf("skipping: PG not reachable or schema failed: %v", err)
	}
	return dsn
}

// --- celeris (native driver/postgres) ---

func startCelerisPGServer(b *testing.B, dsn string) string {
	b.Helper()
	addr := freePort(b)
	srv := celeris.New(celeris.Config{Addr: addr, Logger: quietLogger})

	// Pool is opened AFTER the server starts so WithEngine resolves
	// to the live engine's event loop — if we opened before Start,
	// the server's EventLoopProvider returns nil and the driver
	// falls back to its standalone loop (measured ~9x slower).
	// The handler closure reads the pool through an atomic pointer
	// so route registration works before the pool exists.
	var poolPtr atomic.Pointer[celpostgres.Pool]

	srv.GET("/health", func(c *celeris.Context) error { return c.String(200, "ok") })
	srv.GET("/user", func(c *celeris.Context) error {
		pool := poolPtr.Load()
		if pool == nil {
			return c.String(503, "not ready")
		}
		rows, err := pool.QueryContext(c.Context(), "SELECT id, name FROM bench_users WHERE id=$1", 1)
		if err != nil {
			return c.String(500, "query")
		}
		defer func() { _ = rows.Close() }()
		if !rows.Next() {
			return c.String(404, "no user")
		}
		var u userRow
		if err := rows.Scan(&u.ID, &u.Name); err != nil {
			return c.String(500, "scan")
		}
		return c.JSON(200, u)
	})
	go func() { _ = srv.Start() }()
	b.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	waitReady(b, addr)

	// Now that the server is live, open the pool with WithEngine.
	pool, err := celpostgres.Open(dsn, celpostgres.WithEngine(srv))
	if err != nil {
		b.Fatalf("celeris pg: %v", err)
	}
	b.Cleanup(func() { _ = pool.Close() })
	poolPtr.Store(pool)

	// Warm up. We aggressively warm so each worker in the io_uring /
	// epoll engine has an established PG connection in its idle list
	// — otherwise the first few bench iterations pay a full Parse
	// roundtrip and dominate the measurement. 200 requests with
	// parallel clients covers all 12 workers on a 12-core arm64.
	warmClient := httpClient("warm-pg-" + addr)
	type res struct{ err error }
	done := make(chan res, 200)
	for i := 0; i < 200; i++ {
		go func() {
			resp, err := warmClient.Get("http://" + addr + "/user")
			if err != nil {
				done <- res{err}
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			done <- res{}
		}()
	}
	for i := 0; i < 200; i++ {
		<-done
	}
	return addr
}

func newPgxPool(b *testing.B, dsn string) *pgxpool.Pool {
	b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		b.Fatalf("pgx pool: %v", err)
	}
	b.Cleanup(p.Close)
	return p
}

func startFiberPGServer(b *testing.B, dsn string) string {
	b.Helper()
	pool := newPgxPool(b, dsn)

	app := fiber.New()
	app.Get("/health", func(c fiber.Ctx) error { return c.SendString("ok") })
	app.Get("/user", func(c fiber.Ctx) error {
		var u userRow
		if err := pool.QueryRow(c.RequestCtx(), "SELECT id, name FROM bench_users WHERE id=$1", 1).Scan(&u.ID, &u.Name); err != nil {
			if err == sql.ErrNoRows {
				return c.Status(404).SendString("no user")
			}
			return c.Status(500).SendString("query")
		}
		return c.JSON(u)
	})
	addr := freePort(b)
	go func() { _ = app.Listen(addr, fiber.ListenConfig{DisableStartupMessage: true}) }()
	b.Cleanup(func() { _ = app.Shutdown() })
	waitReady(b, addr)
	return addr
}

func startEchoPGServer(b *testing.B, dsn string) string {
	b.Helper()
	pool := newPgxPool(b, dsn)
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/health", func(c echo.Context) error { return c.String(200, "ok") })
	e.GET("/user", func(c echo.Context) error {
		var u userRow
		if err := pool.QueryRow(c.Request().Context(), "SELECT id, name FROM bench_users WHERE id=$1", 1).Scan(&u.ID, &u.Name); err != nil {
			return c.String(500, "query")
		}
		return c.JSON(200, u)
	})
	addr := freePort(b)
	go func() { _ = e.Start(addr) }()
	b.Cleanup(func() { _ = e.Close() })
	waitReady(b, addr)
	return addr
}

func startChiPGServer(b *testing.B, dsn string) string {
	b.Helper()
	pool := newPgxPool(b, dsn)
	r := chi.NewRouter()
	r.Get("/health", func(w http.ResponseWriter, req *http.Request) { _, _ = w.Write([]byte("ok")) })
	r.Get("/user", func(w http.ResponseWriter, req *http.Request) {
		var u userRow
		if err := pool.QueryRow(req.Context(), "SELECT id, name FROM bench_users WHERE id=$1", 1).Scan(&u.ID, &u.Name); err != nil {
			http.Error(w, "query", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(u)
	})
	addr := freePort(b)
	srv := &http.Server{Addr: addr, Handler: r}
	go func() { _ = srv.ListenAndServe() }()
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	waitReady(b, addr)
	return addr
}

func startStdlibPGServer(b *testing.B, dsn string) string {
	b.Helper()
	pool := newPgxPool(b, dsn)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/user", func(w http.ResponseWriter, req *http.Request) {
		var u userRow
		if err := pool.QueryRow(req.Context(), "SELECT id, name FROM bench_users WHERE id=$1", 1).Scan(&u.ID, &u.Name); err != nil {
			http.Error(w, "query", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(u)
	})
	addr := freePort(b)
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() { _ = srv.ListenAndServe() }()
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	waitReady(b, addr)
	return addr
}

// driveSimpleClients hits GET /user in a tight loop; no special headers.
func driveSimpleClients(b *testing.B, addr, path string) {
	b.Helper()
	url := "http://" + addr + path
	client := httpClient(url)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		resp, err := client.Get(url)
		if err != nil {
			b.Fatalf("req: %v", err)
		}
		_, _ = io.Copy(discardWriter{}, resp.Body)
		_ = resp.Body.Close()
	}
}

// --- benches ---

func BenchmarkPGQuery_Celeris(b *testing.B) {
	dsn := skipIfNoPG(b)
	addr := startCelerisPGServer(b, dsn)
	driveSimpleClients(b, addr, "/user")
}

func BenchmarkPGQuery_Fiber(b *testing.B) {
	dsn := skipIfNoPG(b)
	addr := startFiberPGServer(b, dsn)
	driveSimpleClients(b, addr, "/user")
}

func BenchmarkPGQuery_Echo(b *testing.B) {
	dsn := skipIfNoPG(b)
	addr := startEchoPGServer(b, dsn)
	driveSimpleClients(b, addr, "/user")
}

func BenchmarkPGQuery_Chi(b *testing.B) {
	dsn := skipIfNoPG(b)
	addr := startChiPGServer(b, dsn)
	driveSimpleClients(b, addr, "/user")
}

func BenchmarkPGQuery_Stdlib(b *testing.B) {
	dsn := skipIfNoPG(b)
	addr := startStdlibPGServer(b, dsn)
	driveSimpleClients(b, addr, "/user")
}
