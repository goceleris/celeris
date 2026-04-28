// leak probe: run celeris N times in same process, capture goroutine count + heap stats between runs.
//
// Usage:
//
//	ENG=iouring N=5 DUR=15 go run ./cmd/leakprobe
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/loadgen"
)

func main() {
	engineName := envOr("ENG", "iouring")
	durSec := envInt("DUR", 15)
	conns := envInt("CONNS", 128)
	workers := envInt("WORKERS", 64)
	n := envInt("N", 5)

	var eng celeris.EngineType
	switch engineName {
	case "epoll":
		eng = celeris.Epoll
	case "iouring":
		eng = celeris.IOUring
	case "adaptive":
		eng = celeris.Adaptive
	case "std":
		eng = celeris.Std
	default:
		fmt.Fprintf(os.Stderr, "unknown ENG=%s\n", engineName)
		os.Exit(1)
	}

	for i := 0; i < n; i++ {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		preGo := runtime.NumGoroutine()

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintf(os.Stderr, "listen: %v\n", err)
			os.Exit(1)
		}
		addr := ln.Addr().String()

		cfg := celeris.Config{Addr: addr, Engine: eng, Protocol: celeris.HTTP1}
		srv := celeris.New(cfg)
		srv.GET("/", func(c *celeris.Context) error { return c.String(200, "ok") })

		engineCtx, engineCancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- srv.StartWithListenerAndContext(engineCtx, ln) }()

		dl := time.Now().Add(5 * time.Second)
		for srv.Addr() == nil && time.Now().Before(dl) {
			time.Sleep(2 * time.Millisecond)
		}
		if srv.Addr() == nil {
			engineCancel()
			<-done
			fmt.Fprintf(os.Stderr, "did not bind\n")
			os.Exit(1)
		}
		target := srv.Addr().String()

		bm, lerr := loadgen.New(loadgen.Config{
			URL: "http://" + target + "/", Method: "GET",
			Connections: conns, Workers: workers,
			Duration: time.Duration(durSec) * time.Second,
			Warmup:   2 * time.Second,
		})
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "loadgen.New: %v\n", lerr)
			os.Exit(1)
		}
		res, rerr := bm.Run(context.Background())
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "bm.Run: %v\n", rerr)
		}

		engineCancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}

		// Sleep so goroutines can wind down
		time.Sleep(2 * time.Second)
		runtime.GC()
		var msPost runtime.MemStats
		runtime.ReadMemStats(&msPost)
		postGo := runtime.NumGoroutine()

		fmt.Printf("[run %d] rps=%d p99us=%d preGo=%d postGo=%d preHeapMB=%d postHeapMB=%d numGC=%d\n",
			i,
			int(res.RequestsPerSec),
			res.Latency.P99/1000,
			preGo, postGo,
			ms.HeapInuse/1024/1024,
			msPost.HeapInuse/1024/1024,
			msPost.NumGC-ms.NumGC,
		)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n := 0
	_, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n)
	if err != nil || n == 0 {
		return def
	}
	return n
}
