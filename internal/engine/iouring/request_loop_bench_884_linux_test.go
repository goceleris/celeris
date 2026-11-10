//go:build linux

package iouring

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// BenchmarkRequestLoop884 is the run loop's per-iteration cost end to end
// (celeris#884 items 65, 74 and 75: the sweep gate runs once per loop
// iteration, and mode 3a now arms a standing timer before its untimed wait).
// One op is one request and its response on a keep-alive connection, on one
// worker. "pingpong" is one connection, so the worker is idle between ops and
// every op is a mode-3a SEND wait; "busy" is 32 connections, so the loop is
// rarely idle and the gate is reached at the rate the load sets. rht is the
// ReadHeaderTimeout: the 10 s default (the gate every 32 iterations) or off
// (every 1024). Run it on main and on the head, interleaved, with an A/A twin
// of the same binary (RULE 74).
func BenchmarkRequestLoop884(b *testing.B) {
	for _, conns := range []int{1, 32} {
		for _, rht := range []time.Duration{0, -1} {
			name := map[int]string{1: "pingpong", 32: "busy"}[conns]
			b.Run(fmt.Sprintf("%s/rht=%v", name, rht), func(b *testing.B) {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					b.Fatalf("pick port: %v", err)
				}
				addr := ln.Addr().String()
				_ = ln.Close()
				eng, err := New(resource.Config{
					Addr:              addr,
					Protocol:          engine.HTTP1,
					Resources:         resource.Resources{Workers: 2},
					ReadHeaderTimeout: rht,
					ReadTimeout:       30 * time.Second,
					IdleTimeout:       30 * time.Second,
					WriteTimeout:      30 * time.Second,
				}, newCarry543Handler())
				if err != nil {
					b.Skipf("engine unavailable: %v", err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() { _ = eng.Listen(ctx); close(done) }()
				defer func() { cancel(); <-done }()
				for dl := time.Now().Add(15 * time.Second); (eng.Addr() == nil || eng.NumWorkers() == 0) && time.Now().Before(dl); {
					time.Sleep(2 * time.Millisecond)
				}
				if eng.Addr() == nil {
					b.Fatal("engine did not bind")
				}
				req := []byte("GET /bench HTTP/1.1\r\nHost: x\r\n\r\n")
				type client struct {
					c  net.Conn
					br *bufio.Reader
				}
				clients := make(chan *client, conns)
				for range conns {
					c, err := net.Dial("tcp", eng.Addr().String())
					if err != nil {
						b.Fatalf("dial: %v", err)
					}
					defer func() { _ = c.Close() }()
					clients <- &client{c, bufio.NewReader(c)}
				}
				b.SetParallelism(conns)
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					cl := <-clients
					defer func() { clients <- cl }()
					for pb.Next() {
						if _, err := cl.c.Write(req); err != nil {
							b.Errorf("write: %v", err)
							return
						}
						if _, err := rawResponseLen(cl.br); err != nil {
							b.Errorf("read: %v", err)
							return
						}
					}
				})
			})
		}
	}
}
