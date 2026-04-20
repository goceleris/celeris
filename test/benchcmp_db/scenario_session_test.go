package benchcmp_db

// Scenario A — session-authenticated request.
//
// Setup: a session ID key in Redis maps to a user map {user, role}.
// The benchmark drives HTTP requests carrying the session ID header;
// the server looks the session up in Redis and renders a JSON reply.
//
// This is a realistic "render any authenticated endpoint" scenario.
// Each framework uses its ecosystem-standard Redis client:
//
//   - Celeris: driver/redis (native)
//   - Fiber:   redis/go-redis
//   - Echo:    redis/go-redis
//   - Chi:     redis/go-redis
//   - Stdlib:  redis/go-redis
//
// On missing Redis the benchmark is skipped.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	celeris "github.com/goceleris/celeris"
	celredis "github.com/goceleris/celeris/driver/redis"
	"github.com/gofiber/fiber/v3"
	"github.com/labstack/echo/v4"
	goredis "github.com/redis/go-redis/v9"
)

const sessHeader = "X-Session-ID"
const sessKey = "bench:sess:alice"

// primeSession writes the fixed session blob so every framework sees
// the same data. Called once per scenario.
func primeSession(addr string) error {
	cli := goredis.NewClient(&goredis.Options{Addr: addr})
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	payload := `{"user":"alice","role":"admin"}`
	if _, err := cli.Set(ctx, sessKey, payload, time.Hour).Result(); err != nil {
		return err
	}
	return nil
}

func startCelerisSessionServer(b *testing.B, redisAddr string) string {
	b.Helper()
	addr := freePort(b)
	srv := celeris.New(celeris.Config{Addr: addr, Logger: quietLogger, AsyncHandlers: true})

	// Pool is opened AFTER Start so WithEngine resolves to the live
	// engine's EventLoopProvider; otherwise the driver falls back to
	// its standalone loop (measured ~6-9x slower on arm64 Linux).
	var cliPtr atomic.Pointer[celredis.Client]

	srv.GET("/health", func(c *celeris.Context) error { return c.String(200, "ok") })
	srv.GET("/me", func(c *celeris.Context) error {
		cli := cliPtr.Load()
		if cli == nil {
			return c.String(503, "not ready")
		}
		sid := c.Header(strings.ToLower(sessHeader))
		raw, err := cli.GetBytes(c.Context(), sid)
		if err != nil {
			return c.String(401, "no session")
		}
		var p paddedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return c.String(500, "bad session")
		}
		p.OK = true
		return c.JSON(200, p)
	})
	go func() { _ = srv.Start() }()
	b.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	waitReady(b, addr)

	cli, err := celredis.NewClient(redisAddr, celredis.WithEngine(srv))
	if err != nil {
		b.Fatalf("celeris redis: %v", err)
	}
	b.Cleanup(func() { _ = cli.Close() })
	cliPtr.Store(cli)

	// Warm up every engine worker (up to 12 cores on arm64). Parallel
	// so all workers see a request before the bench starts — otherwise
	// cold worker dispatches pay the first Redis dial.
	warmClient := httpClient("warm-sess-" + addr)
	type res struct{ err error }
	done := make(chan res, 200)
	for i := 0; i < 200; i++ {
		go func() {
			req, _ := http.NewRequest("GET", "http://"+addr+"/me", nil)
			req.Header.Set(sessHeader, sessKey)
			resp, err := warmClient.Do(req)
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

func startFiberSessionServer(b *testing.B, redisAddr string) string {
	b.Helper()
	cli := goredis.NewClient(&goredis.Options{Addr: redisAddr})
	b.Cleanup(func() { _ = cli.Close() })

	app := fiber.New()
	app.Get("/health", func(c fiber.Ctx) error { return c.SendString("ok") })
	app.Get("/me", func(c fiber.Ctx) error {
		sid := c.Get(sessHeader)
		raw, err := cli.Get(c.RequestCtx(), sid).Bytes()
		if err != nil {
			return c.Status(401).SendString("no session")
		}
		var p paddedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return c.Status(500).SendString("bad session")
		}
		p.OK = true
		return c.JSON(p)
	})
	addr := freePort(b)
	go func() { _ = app.Listen(addr, fiber.ListenConfig{DisableStartupMessage: true}) }()
	b.Cleanup(func() { _ = app.Shutdown() })
	waitReady(b, addr)
	return addr
}

func startEchoSessionServer(b *testing.B, redisAddr string) string {
	b.Helper()
	cli := goredis.NewClient(&goredis.Options{Addr: redisAddr})
	b.Cleanup(func() { _ = cli.Close() })

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/health", func(c echo.Context) error { return c.String(200, "ok") })
	e.GET("/me", func(c echo.Context) error {
		sid := c.Request().Header.Get(sessHeader)
		raw, err := cli.Get(c.Request().Context(), sid).Bytes()
		if err != nil {
			return c.String(401, "no session")
		}
		var p paddedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return c.String(500, "bad session")
		}
		p.OK = true
		return c.JSON(200, p)
	})
	addr := freePort(b)
	go func() { _ = e.Start(addr) }()
	b.Cleanup(func() { _ = e.Close() })
	waitReady(b, addr)
	return addr
}

func startChiSessionServer(b *testing.B, redisAddr string) string {
	b.Helper()
	cli := goredis.NewClient(&goredis.Options{Addr: redisAddr})
	b.Cleanup(func() { _ = cli.Close() })

	r := chi.NewRouter()
	r.Get("/health", func(w http.ResponseWriter, req *http.Request) { _, _ = w.Write([]byte("ok")) })
	r.Get("/me", func(w http.ResponseWriter, req *http.Request) {
		sid := req.Header.Get(sessHeader)
		raw, err := cli.Get(req.Context(), sid).Bytes()
		if err != nil {
			http.Error(w, "no session", 401)
			return
		}
		var p paddedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			http.Error(w, "bad session", 500)
			return
		}
		p.OK = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p)
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

func startStdlibSessionServer(b *testing.B, redisAddr string) string {
	b.Helper()
	cli := goredis.NewClient(&goredis.Options{Addr: redisAddr})
	b.Cleanup(func() { _ = cli.Close() })

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/me", func(w http.ResponseWriter, req *http.Request) {
		sid := req.Header.Get(sessHeader)
		raw, err := cli.Get(req.Context(), sid).Bytes()
		if err != nil {
			http.Error(w, "no session", 401)
			return
		}
		var p paddedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			http.Error(w, "bad session", 500)
			return
		}
		p.OK = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p)
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

// driveSessionClients is like driveClients but injects the session
// header so every request matches the primed key.
func driveSessionClients(b *testing.B, addr string) {
	b.Helper()
	url := "http://" + addr + "/me"
	client := httpClient(url)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set(sessHeader, sessKey)
		resp, err := client.Do(req)
		if err != nil {
			b.Fatalf("req: %v", err)
		}
		_, _ = io.CopyN(discardWriter{}, resp.Body, 1<<20)
		_ = resp.Body.Close()
	}
	_ = fmt.Sprintf
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// --- Benchmarks ---

func BenchmarkSession_Celeris(b *testing.B) {
	addr := skipIfNoRedis(b)
	if err := primeSession(addr); err != nil {
		b.Skipf("prime session: %v", err)
	}
	srvAddr := startCelerisSessionServer(b, addr)
	driveSessionClients(b, srvAddr)
}

func BenchmarkSession_Fiber(b *testing.B) {
	addr := skipIfNoRedis(b)
	if err := primeSession(addr); err != nil {
		b.Skipf("prime session: %v", err)
	}
	srvAddr := startFiberSessionServer(b, addr)
	driveSessionClients(b, srvAddr)
}

func BenchmarkSession_Echo(b *testing.B) {
	addr := skipIfNoRedis(b)
	if err := primeSession(addr); err != nil {
		b.Skipf("prime session: %v", err)
	}
	srvAddr := startEchoSessionServer(b, addr)
	driveSessionClients(b, srvAddr)
}

func BenchmarkSession_Chi(b *testing.B) {
	addr := skipIfNoRedis(b)
	if err := primeSession(addr); err != nil {
		b.Skipf("prime session: %v", err)
	}
	srvAddr := startChiSessionServer(b, addr)
	driveSessionClients(b, srvAddr)
}

func BenchmarkSession_Stdlib(b *testing.B) {
	addr := skipIfNoRedis(b)
	if err := primeSession(addr); err != nil {
		b.Skipf("prime session: %v", err)
	}
	srvAddr := startStdlibSessionServer(b, addr)
	driveSessionClients(b, srvAddr)
}
