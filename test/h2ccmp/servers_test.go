// Package h2ccmp benchmarks the RFC 7540 §3.2 HTTP/1.1 → H2C upgrade handshake
// in celeris against the canonical Go baseline: net/http wrapped with
// golang.org/x/net/http2/h2c. Major Go frameworks (gin, echo, chi, fiber/v3)
// all delegate to net/http for H2, so the stdlib+h2c pair is the defensible
// reference. fasthttp / fiber/v2 don't support H2 at all and are outside the
// comparison.
//
// Methodology:
//   - Both servers bind loopback with a fixed no-op GET handler that writes a
//     4-byte "pong" body. Identical handler payload isolates transport cost.
//   - BenchmarkH2CUpgradeHandshake: per iteration, dial a fresh TCP conn,
//     send the H1 Upgrade request + client preface + SETTINGS, read the 101
//     + H2 preface + HEADERS + DATA for stream 1, close. Measures end-to-end
//     handshake cost.
//   - BenchmarkH2CSteadyState: one long-lived conn established via upgrade,
//     then b.N sequential H2 requests over it. Isolates per-request cost
//     post-handshake (decoding, framing, write).
package h2ccmp

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/goceleris/celeris"
)

// silentLogger returns a slog.Logger that swallows output. Celeris logs the
// engine startup banner + warnings; for benchmark runs we don't want those
// to interleave with benchstat-parseable output.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// startCelerisH2C spins up a celeris server with EnableH2Upgrade=true and
// returns its loopback address. Caller must call stop() to shut it down.
// Defaults to the platform default engine (Adaptive on Linux, Std elsewhere).
func startCelerisH2C(tb testing.TB) (addr string, stop func()) {
	tb.Helper()
	return startCelerisH2CEngine(tb, celeris.EngineType(0))
}

// startCelerisH2CEngine is the engine-parameterized variant. Pass 0 for the
// platform default.
func startCelerisH2CEngine(tb testing.TB, eng celeris.EngineType) (addr string, stop func()) {
	tb.Helper()
	yes := true
	cfg := celeris.Config{
		Addr:            "127.0.0.1:0",
		Engine:          eng,
		ShutdownTimeout: 1 * time.Second,
		EnableH2Upgrade: &yes,
		Logger:          silentLogger(),
	}
	srv := celeris.New(cfg)
	srv.GET("/pong", func(c *celeris.Context) error {
		return c.Blob(200, "text/plain", []byte("pong"))
	})
	done := make(chan struct{})
	go func() {
		_ = srv.Start()
		close(done)
	}()
	// Wait for listener.
	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if srv.Addr() == nil {
		tb.Fatal("celeris server did not bind")
	}
	return srv.Addr().String(), func() {
		// Bound shutdown tightly — the celeris custom engines track open
		// connections and will drain before returning. For benches that
		// opened thousands of upgrade conns, a clean drain would dominate
		// the test runtime. 500 ms is enough for the orderly path, and a
		// goroutine-race fallback ensures we don't deadlock if Shutdown
		// itself is unresponsive (server-side teardown bug).
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		shutdownDone := make(chan struct{})
		go func() {
			_ = srv.Shutdown(ctx)
			close(shutdownDone)
		}()
		select {
		case <-shutdownDone:
		case <-time.After(1 * time.Second):
		}
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// startStdH2C spins up an http.Server wrapped with x/net/http2/h2c. This is
// the canonical path used by any net/http-based framework (gin, echo, chi,
// etc.) to serve H2 cleartext with RFC 7540 §3.2 upgrade support.
func startStdH2C(tb testing.TB) (addr string, stop func()) {
	tb.Helper()
	h2s := &http2.Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/pong", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/plain")
		_, _ = w.Write([]byte("pong"))
	})
	handler := h2c.NewHandler(mux, h2s)
	srv := &http.Server{Handler: handler}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ln)
		close(done)
	}()
	return ln.Addr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = ln.Close()
		<-done
	}
}

// parallelBench runs fn concurrently to fill up b.N. The caller supplies the
// per-iteration work. It returns after all iterations complete or b.Fatal is
// called from within fn.
func parallelBench(b *testing.B, fn func(b *testing.B)) {
	var count int64
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			atomic.AddInt64(&count, 1)
			fn(b)
		}
	})
	_ = count
}
