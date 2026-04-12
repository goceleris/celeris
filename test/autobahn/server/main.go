// Package main is a minimal celeris WebSocket echo server used by the
// Autobahn fuzzingclient suite to validate RFC 6455 + RFC 7692
// compliance against each celeris engine (std/epoll/io_uring).
//
// Build:
//
//	go build -o autobahn-server ./test/autobahn/server
//
// Run:
//
//	./autobahn-server -engine=std -addr=:9001
//	./autobahn-server -engine=epoll -addr=:9002
//	./autobahn-server -engine=iouring -addr=:9003
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/websocket"
)

func main() {
	var (
		engineFlag = flag.String("engine", "std", "engine: std | epoll | iouring | adaptive")
		addr       = flag.String("addr", ":9001", "listen address")
	)
	flag.Parse()

	var eng celeris.EngineType
	switch *engineFlag {
	case "std":
		eng = celeris.Std
	case "epoll":
		eng = celeris.Epoll
	case "iouring":
		eng = celeris.IOUring
	case "adaptive":
		eng = celeris.Adaptive
	default:
		log.Fatalf("unknown engine: %s", *engineFlag)
	}

	cfg := celeris.Config{
		Engine: eng,
		Addr:   *addr,
	}
	s := celeris.New(cfg)

	// Single echo route — Autobahn fuzzingclient pounds this with the
	// full case set (1.x–13.x). All cases must pass.
	s.GET("/", websocket.New(websocket.Config{
		EnableCompression: true,
		Handler: func(c *websocket.Conn) {
			for {
				mt, msg, err := c.ReadMessageReuse()
				if err != nil {
					return
				}
				if err := c.WriteMessage(mt, msg); err != nil {
					return
				}
			}
		},
	}))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		fmt.Fprintf(os.Stderr, "celeris autobahn server listening engine=%s addr=%s\n", *engineFlag, *addr)
		if err := s.StartWithContext(ctx); err != nil && err != context.Canceled {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "shutting down")
}
