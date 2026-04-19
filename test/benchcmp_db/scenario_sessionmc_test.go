package benchcmp_db

// Scenario A-mc — session-authenticated request, memcached backend.
// Mirror of scenario_session_test.go but with memcached instead of
// Redis. Celeris uses its native driver/memcached; the four
// competitors use bradfitz/gomemcache — the community-standard Go
// memcached client.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/go-chi/chi/v5"
	celeris "github.com/goceleris/celeris"
	celmc "github.com/goceleris/celeris/driver/memcached"
	"github.com/gofiber/fiber/v3"
	"github.com/labstack/echo/v4"
)

const sessKeyMC = "bench-sess-alice-mc"

func skipIfNoMemcached(b *testing.B) string {
	b.Helper()
	addr := liveMemcachedAddr()
	if !tcpReachable(addr) {
		b.Skipf("skipping: memcached at %s not reachable", addr)
	}
	return addr
}

// primeSessionMC writes the session blob via gomemcache so every
// framework sees the same data.
func primeSessionMC(addr string) error {
	cli := memcache.New(addr)
	cli.Timeout = 2 * time.Second
	return cli.Set(&memcache.Item{
		Key:        sessKeyMC,
		Value:      []byte(`{"user":"alice","role":"admin"}`),
		Expiration: 3600,
	})
}

func startCelerisSessionMCServer(b *testing.B, mcAddr string) string {
	b.Helper()
	addr := freePort(b)
	srv := celeris.New(celeris.Config{Addr: addr, Logger: quietLogger})

	var cliPtr atomic.Pointer[celmc.Client]
	srv.GET("/health", func(c *celeris.Context) error { return c.String(200, "ok") })
	srv.GET("/me", func(c *celeris.Context) error {
		cli := cliPtr.Load()
		sid := c.Header(strings.ToLower(sessHeader))
		raw, err := cli.GetBytes(c.Context(), sid)
		if err != nil {
			return c.String(401, "no session")
		}
		var p paddedJSON
		if err := json.Unmarshal(raw, &p); err != nil {
			return c.String(500, "bad")
		}
		p.OK = true
		return c.JSON(200, p)
	})
	go func() { _ = srv.Start() }()
	b.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	waitReady(b, addr)

	cli, err := celmc.NewClient(mcAddr)
	if err != nil {
		b.Fatalf("celeris mc: %v", err)
	}
	b.Cleanup(func() { _ = cli.Close() })
	cliPtr.Store(cli)

	warmAllWorkersMC(addr, sessHeader, sessKeyMC)
	return addr
}

func startFiberSessionMCServer(b *testing.B, mcAddr string) string {
	b.Helper()
	cli := memcache.New(mcAddr)
	cli.Timeout = 2 * time.Second

	app := fiber.New()
	app.Get("/health", func(c fiber.Ctx) error { return c.SendString("ok") })
	app.Get("/me", func(c fiber.Ctx) error {
		sid := c.Get(sessHeader)
		item, err := cli.Get(sid)
		if err != nil {
			return c.Status(401).SendString("no session")
		}
		var p paddedJSON
		if err := json.Unmarshal(item.Value, &p); err != nil {
			return c.Status(500).SendString("bad")
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

func startEchoSessionMCServer(b *testing.B, mcAddr string) string {
	b.Helper()
	cli := memcache.New(mcAddr)
	cli.Timeout = 2 * time.Second
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/health", func(c echo.Context) error { return c.String(200, "ok") })
	e.GET("/me", func(c echo.Context) error {
		sid := c.Request().Header.Get(sessHeader)
		item, err := cli.Get(sid)
		if err != nil {
			return c.String(401, "no session")
		}
		var p paddedJSON
		if err := json.Unmarshal(item.Value, &p); err != nil {
			return c.String(500, "bad")
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

func startChiSessionMCServer(b *testing.B, mcAddr string) string {
	b.Helper()
	cli := memcache.New(mcAddr)
	cli.Timeout = 2 * time.Second
	r := chi.NewRouter()
	r.Get("/health", func(w http.ResponseWriter, req *http.Request) { _, _ = w.Write([]byte("ok")) })
	r.Get("/me", func(w http.ResponseWriter, req *http.Request) {
		sid := req.Header.Get(sessHeader)
		item, err := cli.Get(sid)
		if err != nil {
			http.Error(w, "no session", 401)
			return
		}
		var p paddedJSON
		if err := json.Unmarshal(item.Value, &p); err != nil {
			http.Error(w, "bad", 500)
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

func startStdlibSessionMCServer(b *testing.B, mcAddr string) string {
	b.Helper()
	cli := memcache.New(mcAddr)
	cli.Timeout = 2 * time.Second
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/me", func(w http.ResponseWriter, req *http.Request) {
		sid := req.Header.Get(sessHeader)
		item, err := cli.Get(sid)
		if err != nil {
			http.Error(w, "no session", 401)
			return
		}
		var p paddedJSON
		if err := json.Unmarshal(item.Value, &p); err != nil {
			http.Error(w, "bad", 500)
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

// warmAllWorkersMC is a session-header-aware parallel warmup that
// mirrors the celeris Redis variant.
func warmAllWorkersMC(addr, header, key string) {
	wc := httpClient("warm-mc-" + addr)
	done := make(chan struct{}, 200)
	for i := 0; i < 200; i++ {
		go func() {
			req, _ := http.NewRequest("GET", "http://"+addr+"/me", nil)
			req.Header.Set(header, key)
			resp, err := wc.Do(req)
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
}

// driveSessionMCClients drives /me with the memcached session key.
func driveSessionMCClients(b *testing.B, addr string) {
	b.Helper()
	url := "http://" + addr + "/me"
	client := httpClient(url)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set(sessHeader, sessKeyMC)
		resp, err := client.Do(req)
		if err != nil {
			b.Fatalf("req: %v", err)
		}
		_, _ = io.CopyN(discardWriter{}, resp.Body, 1<<20)
		_ = resp.Body.Close()
	}
}

// --- Benches ---

func BenchmarkSessionMC_Celeris(b *testing.B) {
	addr := skipIfNoMemcached(b)
	if err := primeSessionMC(addr); err != nil {
		b.Skipf("prime: %v", err)
	}
	srv := startCelerisSessionMCServer(b, addr)
	driveSessionMCClients(b, srv)
}
func BenchmarkSessionMC_Fiber(b *testing.B) {
	addr := skipIfNoMemcached(b)
	if err := primeSessionMC(addr); err != nil {
		b.Skipf("prime: %v", err)
	}
	srv := startFiberSessionMCServer(b, addr)
	driveSessionMCClients(b, srv)
}
func BenchmarkSessionMC_Echo(b *testing.B) {
	addr := skipIfNoMemcached(b)
	if err := primeSessionMC(addr); err != nil {
		b.Skipf("prime: %v", err)
	}
	srv := startEchoSessionMCServer(b, addr)
	driveSessionMCClients(b, srv)
}
func BenchmarkSessionMC_Chi(b *testing.B) {
	addr := skipIfNoMemcached(b)
	if err := primeSessionMC(addr); err != nil {
		b.Skipf("prime: %v", err)
	}
	srv := startChiSessionMCServer(b, addr)
	driveSessionMCClients(b, srv)
}
func BenchmarkSessionMC_Stdlib(b *testing.B) {
	addr := skipIfNoMemcached(b)
	if err := primeSessionMC(addr); err != nil {
		b.Skipf("prime: %v", err)
	}
	srv := startStdlibSessionMCServer(b, addr)
	driveSessionMCClients(b, srv)
}
