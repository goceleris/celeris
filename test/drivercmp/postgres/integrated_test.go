//go:build linux

package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	celeris "github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func keepAliveClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
			DisableKeepAlives:   false,
		},
	}
}

func doGet(b *testing.B, client *http.Client, url string) {
	b.Helper()
	resp, err := client.Get(url)
	if err != nil {
		b.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b.Fatalf("status = %d", resp.StatusCode)
	}
}

func warmup(b *testing.B, client *http.Client, url string, n int) {
	b.Helper()
	for i := 0; i < n; i++ {
		doGet(b, client, url)
	}
}

// startCelerisServer creates a celeris server that stays alive until the
// returned cancel function is called. The caller must call cancel when done.
func startCelerisServer(t testing.TB, engineType celeris.EngineType, handler celeris.HandlerFunc) (srv *celeris.Server, baseURL string, cancel func()) {
	t.Helper()
	srv = celeris.New(celeris.Config{Engine: engineType})
	srv.GET("/bench", handler)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, ctxCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.StartWithListenerAndContext(ctx, ln) }()

	deadline := time.Now().Add(5 * time.Second)
	for srv.Addr() == nil && time.Now().Before(deadline) {
		select {
		case err := <-done:
			ctxCancel()
			t.Fatalf("server exited early: %v", err)
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	if srv.Addr() == nil {
		ctxCancel()
		t.Fatal("server did not bind within deadline")
	}

	cancel = func() {
		ctxCancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	baseURL = "http://" + srv.Addr().String()
	return srv, baseURL, cancel
}

// ---------------------------------------------------------------------------
// Celeris integrated (WithEngine) benchmarks
//
// Each uses b.Run so that the outer function handles server lifecycle once
// while the inner sub-benchmark handles b.N calibration.
// ---------------------------------------------------------------------------

func BenchmarkIntegrated_Celeris_Epoll(b *testing.B) {
	benchmarkIntegratedCeleris(b, celeris.Epoll)
}

func BenchmarkIntegrated_Celeris_IOUring(b *testing.B) {
	benchmarkIntegratedCeleris(b, celeris.IOUring)
}

func benchmarkIntegratedCeleris(b *testing.B, engineType celeris.EngineType) {
	d := dsn(b)
	var pool *postgres.Pool

	srv, base, stop := startCelerisServer(b, engineType, func(c *celeris.Context) error {
		rows, err := pool.QueryContext(c.Context(), "SELECT 1")
		if err != nil {
			return c.String(http.StatusInternalServerError, "query: %v", err)
		}
		if !rows.Next() {
			rows.Close()
			return c.String(http.StatusInternalServerError, "no row")
		}
		var val any
		if err := rows.Scan(&val); err != nil {
			rows.Close()
			return c.String(http.StatusInternalServerError, "scan: %v", err)
		}
		rows.Close()
		return c.String(http.StatusOK, "%v", val)
	})
	defer stop()

	var err error
	pool, err = postgres.Open(d, postgres.WithEngine(srv), postgres.WithMaxOpen(64))
	if err != nil {
		b.Fatalf("postgres.Open: %v", err)
	}
	defer func() { _ = pool.Close() }()

	url := base + "/bench"
	client := keepAliveClient()
	warmup(b, client, url, 10)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doGet(b, client, url)
	}
}

// ---------------------------------------------------------------------------
// net/http + pgx
// ---------------------------------------------------------------------------

func BenchmarkIntegrated_Pgx_NetHTTP(b *testing.B) {
	d := dsn(b)
	ctx := context.Background()

	pgxPool, err := pgxpool.New(ctx, d)
	if err != nil {
		b.Fatalf("pgxpool.New: %v", err)
	}
	defer pgxPool.Close()
	if err := pgxPool.Ping(ctx); err != nil {
		b.Skipf("pgx ping: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/bench", func(w http.ResponseWriter, r *http.Request) {
		row := pgxPool.QueryRow(r.Context(), "SELECT 1")
		var v int
		if err := row.Scan(&v); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "%d", v)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	stdSrv := &http.Server{Handler: mux}
	go func() { _ = stdSrv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stdSrv.Shutdown(shutCtx)
	}()

	client := keepAliveClient()
	url := "http://" + ln.Addr().String() + "/bench"
	warmup(b, client, url, 10)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doGet(b, client, url)
	}
}

// ---------------------------------------------------------------------------
// net/http + lib/pq
// ---------------------------------------------------------------------------

func BenchmarkIntegrated_LibPQ_NetHTTP(b *testing.B) {
	d := dsn(b)
	ctx := context.Background()

	db, err := sql.Open("postgres", d)
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(64)
	db.SetMaxIdleConns(32)
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		b.Skipf("lib/pq ping: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/bench", func(w http.ResponseWriter, r *http.Request) {
		var v int
		if err := db.QueryRowContext(r.Context(), "SELECT 1").Scan(&v); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "%d", v)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	stdSrv := &http.Server{Handler: mux}
	go func() { _ = stdSrv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stdSrv.Shutdown(shutCtx)
	}()

	client := keepAliveClient()
	url := "http://" + ln.Addr().String() + "/bench"
	warmup(b, client, url, 10)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		doGet(b, client, url)
	}
}

// ---------------------------------------------------------------------------
// Parallel variants
// ---------------------------------------------------------------------------

func BenchmarkIntegratedParallel_Celeris_Epoll(b *testing.B) {
	benchmarkIntegratedParallelCeleris(b, celeris.Epoll)
}

func BenchmarkIntegratedParallel_Celeris_IOUring(b *testing.B) {
	benchmarkIntegratedParallelCeleris(b, celeris.IOUring)
}

func benchmarkIntegratedParallelCeleris(b *testing.B, engineType celeris.EngineType) {
	d := dsn(b)
	var pool *postgres.Pool

	srv, base, stop := startCelerisServer(b, engineType, func(c *celeris.Context) error {
		rows, err := pool.QueryContext(c.Context(), "SELECT 1")
		if err != nil {
			return c.String(http.StatusInternalServerError, "query: %v", err)
		}
		if !rows.Next() {
			rows.Close()
			return c.String(http.StatusInternalServerError, "no row")
		}
		var val any
		if err := rows.Scan(&val); err != nil {
			rows.Close()
			return c.String(http.StatusInternalServerError, "scan: %v", err)
		}
		rows.Close()
		return c.String(http.StatusOK, "%v", val)
	})
	defer stop()

	var err error
	pool, err = postgres.Open(d, postgres.WithEngine(srv), postgres.WithMaxOpen(64))
	if err != nil {
		b.Fatalf("postgres.Open: %v", err)
	}
	defer func() { _ = pool.Close() }()

	url := base + "/bench"
	warmup(b, keepAliveClient(), url, 10)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		c := keepAliveClient()
		for pb.Next() {
			doGet(b, c, url)
		}
	})
}

func BenchmarkIntegratedParallel_Pgx_NetHTTP(b *testing.B) {
	d := dsn(b)
	ctx := context.Background()

	pgxPool, err := pgxpool.New(ctx, d)
	if err != nil {
		b.Fatalf("pgxpool.New: %v", err)
	}
	defer pgxPool.Close()
	if err := pgxPool.Ping(ctx); err != nil {
		b.Skipf("pgx ping: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/bench", func(w http.ResponseWriter, r *http.Request) {
		row := pgxPool.QueryRow(r.Context(), "SELECT 1")
		var v int
		if err := row.Scan(&v); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "%d", v)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	stdSrv := &http.Server{Handler: mux}
	go func() { _ = stdSrv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stdSrv.Shutdown(shutCtx)
	}()

	url := "http://" + ln.Addr().String() + "/bench"
	warmup(b, keepAliveClient(), url, 10)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		c := keepAliveClient()
		for pb.Next() {
			doGet(b, c, url)
		}
	})
}

func BenchmarkIntegratedParallel_LibPQ_NetHTTP(b *testing.B) {
	d := dsn(b)
	ctx := context.Background()

	db, err := sql.Open("postgres", d)
	if err != nil {
		b.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(64)
	db.SetMaxIdleConns(32)
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		b.Skipf("lib/pq ping: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/bench", func(w http.ResponseWriter, r *http.Request) {
		var v int
		if err := db.QueryRowContext(r.Context(), "SELECT 1").Scan(&v); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Fprintf(w, "%d", v)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	stdSrv := &http.Server{Handler: mux}
	go func() { _ = stdSrv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stdSrv.Shutdown(shutCtx)
	}()

	url := "http://" + ln.Addr().String() + "/bench"
	warmup(b, keepAliveClient(), url, 10)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		c := keepAliveClient()
		for pb.Next() {
			doGet(b, c, url)
		}
	})
}
