// Package benchmark provides performance benchmarks for Celeris engines.
package benchmark

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	stdengine "github.com/goceleris/celeris/engine/std"
	"github.com/goceleris/celeris/resource"
)

func freePort(b *testing.B) int {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("find free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func setupBenchEngine(b *testing.B) (string, func()) {
	b.Helper()

	port := freePort(b)
	cfg := resource.Config{
		Addr:     fmt.Sprintf(":%d", port),
		Engine:   engine.Std,
		Protocol: engine.HTTP1,
	}

	e, err := stdengine.New(cfg, &benchHandler{})
	if err != nil {
		b.Fatalf("create engine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- e.Listen(ctx)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for e.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if e.Addr() == nil {
		cancel()
		b.Fatal("engine did not start")
	}

	return e.Addr().String(), func() {
		cancel()
		<-errCh
	}
}

func BenchmarkPlaintext(b *testing.B) {
	addr, cleanup := setupBenchEngine(b)
	defer cleanup()

	client := &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 1,
			DialContext: (&net.Dialer{
				Timeout: 5 * time.Second,
			}).DialContext,
		},
	}

	b.ResetTimer()
	for b.Loop() {
		resp, err := client.Get("http://" + addr + "/")
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func BenchmarkJSON(b *testing.B) {
	addr, cleanup := setupBenchEngine(b)
	defer cleanup()

	client := &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: 1},
	}

	b.ResetTimer()
	for b.Loop() {
		resp, err := client.Get("http://" + addr + "/json")
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func BenchmarkPathParam(b *testing.B) {
	addr, cleanup := setupBenchEngine(b)
	defer cleanup()

	client := &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: 1},
	}

	b.ResetTimer()
	for b.Loop() {
		resp, err := client.Get("http://" + addr + "/users/123")
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func BenchmarkBodyRead(b *testing.B) {
	addr, cleanup := setupBenchEngine(b)
	defer cleanup()

	client := &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: 1},
	}
	body := bytes.Repeat([]byte("x"), 1024)

	b.ResetTimer()
	for b.Loop() {
		resp, err := client.Post("http://"+addr+"/upload", "application/octet-stream", bytes.NewReader(body))
		if err != nil {
			b.Fatalf("POST: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func BenchmarkRealisticAPI(b *testing.B) {
	addr, cleanup := setupBenchEngine(b)
	defer cleanup()

	client := &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: 1},
	}

	b.ResetTimer()
	for b.Loop() {
		req, _ := http.NewRequest("GET", "http://"+addr+"/users/123", nil)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-Request-ID", "bench-123")
		req.Header.Set("User-Agent", "celeris-bench/1.0")
		req.Header.Set("Accept-Encoding", "gzip")

		resp, err := client.Do(req)
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}
