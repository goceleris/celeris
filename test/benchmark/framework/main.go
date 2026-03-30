//go:build linux

// Package main runs a standalone celeris.Server for external benchmarking
// with wrk, hey, or other load generators. Uses full framework (Router,
// Context, middleware chain) for measuring framework overhead vs raw server.
package main

import (
	"context"
	"log"
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
	protoName := envOr("PROTOCOL", "h1")
	port := envOr("PORT", "18080")

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

	var proto celeris.Protocol
	switch protoName {
	case "h1":
		proto = celeris.HTTP1
	case "h2":
		proto = celeris.H2C
	case "hybrid", "auto":
		proto = celeris.Auto
	default:
		log.Fatalf("unknown protocol: %s", protoName)
	}

	s := celeris.New(celeris.Config{
		Addr:           ":" + port,
		Protocol:       proto,
		Engine:         eng,
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

	log.Printf("Starting framework %s-%s on :%s", engName, protoName, port)
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
