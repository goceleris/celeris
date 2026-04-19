package benchcmp_db

// Scenario C — cache-backed user lookup (Redis GET per request).
//
// Setup: a user record is cached in Redis; every request GETs it and
// renders the JSON. This isolates "framework + Redis GET + JSON
// marshal" so the numbers reflect the minimal realistic API read path
// (no session lookup ceremony, no DB). The four framework
// implementations use go-redis; celeris uses its native driver.

import (
	"context"
	"encoding/json"
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

const cacheKey = "bench:user:42"

func primeCacheValue(addr string) error {
	cli := goredis.NewClient(&goredis.Options{Addr: addr})
	defer func() { _ = cli.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	payload := `{"id":42,"name":"alice","plan":"pro"}`
	_, err := cli.Set(ctx, cacheKey, payload, time.Hour).Result()
	return err
}

type cachedUser struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Plan string `json:"plan"`
}

func startCelerisCacheServer(b *testing.B, redisAddr string) string {
	b.Helper()
	addr := freePort(b)
	srv := celeris.New(celeris.Config{Addr: addr, Logger: quietLogger})

	var cliPtr atomic.Pointer[celredis.Client]
	srv.GET("/health", func(c *celeris.Context) error { return c.String(200, "ok") })
	srv.GET("/user", func(c *celeris.Context) error {
		cli := cliPtr.Load()
		raw, err := cli.GetBytes(c.Context(), cacheKey)
		if err != nil {
			return c.String(404, "miss")
		}
		var u cachedUser
		if err := json.Unmarshal(raw, &u); err != nil {
			return c.String(500, "bad")
		}
		return c.JSON(200, u)
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
	// Warm up every worker in parallel.
	wc := httpClient("warm-cache-" + addr)
	done := make(chan struct{}, 200)
	for i := 0; i < 200; i++ {
		go func() {
			resp, err := wc.Get("http://" + addr + "/user")
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 200; i++ {
		<-done
	}
	return addr
}

func startFiberCacheServer(b *testing.B, redisAddr string) string {
	b.Helper()
	cli := goredis.NewClient(&goredis.Options{Addr: redisAddr})
	b.Cleanup(func() { _ = cli.Close() })
	app := fiber.New()
	app.Get("/health", func(c fiber.Ctx) error { return c.SendString("ok") })
	app.Get("/user", func(c fiber.Ctx) error {
		raw, err := cli.Get(c.RequestCtx(), cacheKey).Bytes()
		if err != nil {
			return c.Status(404).SendString("miss")
		}
		var u cachedUser
		if err := json.Unmarshal(raw, &u); err != nil {
			return c.Status(500).SendString("bad")
		}
		return c.JSON(u)
	})
	addr := freePort(b)
	go func() { _ = app.Listen(addr, fiber.ListenConfig{DisableStartupMessage: true}) }()
	b.Cleanup(func() { _ = app.Shutdown() })
	waitReady(b, addr)
	return addr
}

func startEchoCacheServer(b *testing.B, redisAddr string) string {
	b.Helper()
	cli := goredis.NewClient(&goredis.Options{Addr: redisAddr})
	b.Cleanup(func() { _ = cli.Close() })
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/health", func(c echo.Context) error { return c.String(200, "ok") })
	e.GET("/user", func(c echo.Context) error {
		raw, err := cli.Get(c.Request().Context(), cacheKey).Bytes()
		if err != nil {
			return c.String(404, "miss")
		}
		var u cachedUser
		if err := json.Unmarshal(raw, &u); err != nil {
			return c.String(500, "bad")
		}
		return c.JSON(200, u)
	})
	addr := freePort(b)
	go func() { _ = e.Start(addr) }()
	b.Cleanup(func() { _ = e.Close() })
	waitReady(b, addr)
	return addr
}

func startChiCacheServer(b *testing.B, redisAddr string) string {
	b.Helper()
	cli := goredis.NewClient(&goredis.Options{Addr: redisAddr})
	b.Cleanup(func() { _ = cli.Close() })
	r := chi.NewRouter()
	r.Get("/health", func(w http.ResponseWriter, req *http.Request) { _, _ = w.Write([]byte("ok")) })
	r.Get("/user", func(w http.ResponseWriter, req *http.Request) {
		raw, err := cli.Get(req.Context(), cacheKey).Bytes()
		if err != nil {
			http.Error(w, "miss", 404)
			return
		}
		var u cachedUser
		if err := json.Unmarshal(raw, &u); err != nil {
			http.Error(w, "bad", 500)
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

func startStdlibCacheServer(b *testing.B, redisAddr string) string {
	b.Helper()
	cli := goredis.NewClient(&goredis.Options{Addr: redisAddr})
	b.Cleanup(func() { _ = cli.Close() })
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/user", func(w http.ResponseWriter, req *http.Request) {
		raw, err := cli.Get(req.Context(), cacheKey).Bytes()
		if err != nil {
			http.Error(w, "miss", 404)
			return
		}
		var u cachedUser
		if err := json.Unmarshal(raw, &u); err != nil {
			http.Error(w, "bad", 500)
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

// --- Benches ---

func BenchmarkCacheGet_Celeris(b *testing.B) {
	addr := skipIfNoRedis(b)
	if err := primeCacheValue(addr); err != nil {
		b.Skipf("prime: %v", err)
	}
	srv := startCelerisCacheServer(b, addr)
	driveSimpleClients(b, srv, "/user")
}
func BenchmarkCacheGet_Fiber(b *testing.B) {
	addr := skipIfNoRedis(b)
	if err := primeCacheValue(addr); err != nil {
		b.Skipf("prime: %v", err)
	}
	srv := startFiberCacheServer(b, addr)
	driveSimpleClients(b, srv, "/user")
}
func BenchmarkCacheGet_Echo(b *testing.B) {
	addr := skipIfNoRedis(b)
	if err := primeCacheValue(addr); err != nil {
		b.Skipf("prime: %v", err)
	}
	srv := startEchoCacheServer(b, addr)
	driveSimpleClients(b, srv, "/user")
}
func BenchmarkCacheGet_Chi(b *testing.B) {
	addr := skipIfNoRedis(b)
	if err := primeCacheValue(addr); err != nil {
		b.Skipf("prime: %v", err)
	}
	srv := startChiCacheServer(b, addr)
	driveSimpleClients(b, srv, "/user")
}
func BenchmarkCacheGet_Stdlib(b *testing.B) {
	addr := skipIfNoRedis(b)
	if err := primeCacheValue(addr); err != nil {
		b.Skipf("prime: %v", err)
	}
	srv := startStdlibCacheServer(b, addr)
	driveSimpleClients(b, srv, "/user")
}

// Keep strings import alive for session scenario file that shares package.
var _ = strings.HasPrefix
