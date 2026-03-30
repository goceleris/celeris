//go:build linux

// Package main runs a standalone celeris server for external benchmarking
// with wrk, hey, or other load generators. Uses stream.Handler directly
// (bypasses celeris Server/Router) for raw engine performance testing.
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/goceleris/celeris/adaptive"
	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/epoll"
	"github.com/goceleris/celeris/engine/iouring"
	"github.com/goceleris/celeris/engine/std"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

func main() {
	engName := envOr("ENGINE", "iouring")
	protoName := envOr("PROTOCOL", "h1")
	port := envOr("PORT", "18080")

	var proto engine.Protocol
	switch protoName {
	case "h1":
		proto = engine.HTTP1
	case "h2":
		proto = engine.H2C
	case "hybrid", "auto":
		proto = engine.Auto
	default:
		log.Fatalf("unknown protocol: %s", protoName)
	}

	cfg := resource.Config{
		Addr:     ":" + port,
		Protocol: proto,
	}.WithDefaults()

	// Override workers if explicitly set.
	if w := envOr("WORKERS", ""); w != "" {
		var n int
		if _, err := fmt.Sscanf(w, "%d", &n); err == nil && n > 0 {
			cfg.Resources.Workers = n
		}
	}

	handler := newHandler()
	eng, err := createEngine(engName, cfg, handler)
	if err != nil {
		log.Fatalf("engine create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
	}()

	log.Printf("Starting %s-%s on :%s", engName, protoName, port)
	if err := eng.Listen(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("listen: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func createEngine(name string, cfg resource.Config, handler stream.Handler) (engine.Engine, error) {
	switch name {
	case "iouring":
		return iouring.New(cfg, handler)
	case "epoll":
		return epoll.New(cfg, handler)
	case "adaptive":
		return adaptive.New(cfg, handler)
	case "std":
		return std.New(cfg, handler)
	default:
		return nil, fmt.Errorf("unknown engine: %s", name)
	}
}

// Pre-allocated response components to minimize benchmark handler overhead.
var (
	textPlainHeaders = [][2]string{{"content-type", "text/plain"}}
	jsonHeaders      = [][2]string{{"content-type", "application/json"}}
	octetHeaders     = [][2]string{{"content-type", "application/octet-stream"}}
	helloBody        = []byte("Hello, World!")
	jsonBody         = []byte(`{"message":"Hello, World!"}`)
	notFoundBody     = []byte("Not Found")
	downloadBody     = bytes.Repeat([]byte("X"), 65536) // 64KB for large-download benchmarks
)

func newHandler() stream.HandlerFunc {
	return func(_ context.Context, s *stream.Stream) error {
		defer s.Cancel()

		// H1 pseudo-headers are always at fixed positions:
		// [0] = :method, [1] = :path, [2] = :scheme, [3] = :authority
		// For H2, the same convention is maintained by HPACK decoding.
		// Direct index access eliminates the header iteration loop.
		headers := s.GetHeaders()
		if len(headers) < 2 {
			return s.ResponseWriter.WriteResponse(s, 400, textPlainHeaders, notFoundBody)
		}
		method := headers[0][1]
		path := headers[1][1]

		switch {
		case method == "GET" && path == "/":
			return s.ResponseWriter.WriteResponse(s, 200, textPlainHeaders, helloBody)
		case method == "GET" && path == "/json":
			return s.ResponseWriter.WriteResponse(s, 200, jsonHeaders, jsonBody)
		case method == "GET" && path == "/download":
			return s.ResponseWriter.WriteResponse(s, 200, octetHeaders, downloadBody)
		case method == "GET" && strings.HasPrefix(path, "/users/"):
			id := strings.TrimPrefix(path, "/users/")
			return s.ResponseWriter.WriteResponse(s, 200, textPlainHeaders, []byte("User ID: "+id))
		case method == "POST" && path == "/upload":
			// Body benchmark: read and discard request body, return OK.
			return s.ResponseWriter.WriteResponse(s, 200, textPlainHeaders, helloBody)
		default:
			return s.ResponseWriter.WriteResponse(s, 404, textPlainHeaders, notFoundBody)
		}
	}
}
