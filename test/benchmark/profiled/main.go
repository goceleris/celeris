//go:build linux

// Package main runs a celeris framework server with pprof profiling enabled.
// Usage: start server, run wrk, then curl http://localhost:6060/debug/pprof/profile?seconds=10
package main

import (
	"context"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"

	"github.com/goceleris/celeris"
)

type jsonMsg struct {
	Message string `json:"message"`
}

func main() {
	engName := envOr("ENGINE", "iouring")
	objName := envOr("OBJECTIVE", "latency")
	port := envOr("PORT", "18080")

	// pprof server on separate port
	go func() {
		log.Println("pprof listening on :6060")
		_ = http.ListenAndServe(":6060", nil)
	}()

	var eng celeris.EngineType
	switch engName {
	case "iouring":
		eng = celeris.IOUring
	case "epoll":
		eng = celeris.Epoll
	case "std":
		eng = celeris.Std
	default:
		log.Fatalf("unknown engine: %s", engName)
	}

	var obj celeris.Objective
	switch objName {
	case "latency":
		obj = celeris.Latency
	case "throughput":
		obj = celeris.Throughput
	case "balanced":
		obj = celeris.Balanced
	default:
		log.Fatalf("unknown objective: %s", objName)
	}

	s := celeris.New(celeris.Config{
		Addr:           ":" + port,
		Protocol:       celeris.HTTP1,
		Engine:         eng,
		Objective:      obj,
		DisableMetrics: true,
	})

	s.GET("/", func(c *celeris.Context) error {
		return c.String(200, "Hello, World!")
	})
	s.GET("/json", func(c *celeris.Context) error {
		return c.JSON(200, jsonMsg{Message: "Hello, World!"})
	})
	s.GET("/users/:id", func(c *celeris.Context) error {
		return c.String(200, "%s", "User ID: "+c.Param("id"))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
	}()

	log.Printf("Starting profiled %s-%s on :%s", engName, objName, port)
	if err := s.StartWithContext(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("listen: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
