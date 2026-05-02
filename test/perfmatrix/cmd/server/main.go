// Command server is a thin wrapper that runs ONE registered perfmatrix
// celeris server in standalone mode, bound to a network-reachable
// address. Used by the cluster distributed bench (loadgen on
// msa2-client → server on msa2-server / msr1).
//
// All scenario handlers, engine wiring, h2c-upgrade probing, etc. are
// reused as-is from the perfmatrix registry — this binary just looks up
// the named server, starts it on the requested bind, prints
// "perfmatrix-server: ready addr=<host:port>" to stdout, and waits for
// SIGINT/SIGTERM for graceful shutdown.
//
// Driver scenarios are intentionally NOT supported here: passing a nil
// services.Handles makes driver routes return 503 without needing
// Postgres/Redis/Memcached on the bench host. Static and chain
// scenarios work end-to-end.
//
// Usage:
//
//	server -name celeris-iouring-h1-async -bind 0.0.0.0:8080
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
	_ "github.com/goceleris/celeris/test/perfmatrix/servers/celeris"
)

func main() {
	name := flag.String("name", "", "registered server name (e.g. celeris-iouring-h1-async). Required.")
	bind := flag.String("bind", "0.0.0.0:8080", "address to bind the listener (host:port)")
	flag.Parse()

	if *name == "" {
		flag.Usage()
		log.Fatal("perfmatrix-server: -name is required")
	}

	if err := os.Setenv("CELERIS_PERFMATRIX_BIND", *bind); err != nil {
		log.Fatalf("perfmatrix-server: setenv: %v", err)
	}

	var srv servers.Server
	for _, s := range servers.Registry() {
		if s.Name() == *name {
			srv = s
			break
		}
	}
	if srv == nil {
		log.Fatalf("perfmatrix-server: unknown server %q. Available: see Registry()", *name)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln, err := srv.Start(ctx, nil)
	if err != nil {
		log.Fatalf("perfmatrix-server: Start failed: %v", err)
	}

	// Stable line that the orchestrator scrapes for "ready" and to learn
	// the actual address (matters when -bind ":0" picks an ephemeral port).
	fmt.Printf("perfmatrix-server: ready addr=%s name=%s\n", ln.Addr().String(), *name)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("perfmatrix-server: shutdown signal received")

	stopCtx, stopCancel := context.WithCancel(context.Background())
	defer stopCancel()
	if err := srv.Stop(stopCtx); err != nil {
		log.Printf("perfmatrix-server: Stop returned: %v", err)
	}
	fmt.Println("perfmatrix-server: stopped")
}
