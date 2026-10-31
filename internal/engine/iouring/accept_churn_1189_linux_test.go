//go:build linux

package iouring

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// BenchmarkAcceptChurnAuto1189 is the cost of arming the header timer of a
// connection at its accept, which is a connection whose protocol is not decided
// yet (celeris#1189): Protocol Auto, as the default is, with ReadHeaderTimeout
// set (the default is 10 s) or off (the control: the change is not reached). One
// op is one connection, on 8 client goroutines and one worker. "request" sends one
// request with Connection: close and reads the response to the end: the timer
// armed at the accept is the one the request would have armed at detection, so
// this arm must not move. "bare" connects and closes without a byte: it pays the
// arm and the cancel the request arm did not, so it is the worst case for the
// change. The kernel's connect and close dominate each op, so read the figures
// against an A/A twin of the same binary (RULE 74).
func BenchmarkAcceptChurnAuto1189(b *testing.B) {
	for _, rht := range []time.Duration{10 * time.Second, -1} {
		for _, arm := range []string{"request", "bare"} {
			b.Run(fmt.Sprintf("%s/rht=%v", arm, rht), func(b *testing.B) {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					b.Fatalf("pick port: %v", err)
				}
				addr := ln.Addr().String()
				_ = ln.Close()
				e, err := New(resource.Config{
					Addr:              addr,
					Protocol:          engine.Auto,
					Resources:         resource.Resources{Workers: 2},
					ReadHeaderTimeout: rht,
					Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
				}, newCarry543Handler())
				if err != nil {
					b.Skipf("iouring engine unavailable: %v", err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() { _ = e.Listen(ctx); close(done) }()
				defer func() { cancel(); <-done }()
				for dl := time.Now().Add(15 * time.Second); (e.Addr() == nil || e.NumWorkers() == 0) && time.Now().Before(dl); {
					time.Sleep(2 * time.Millisecond)
				}
				if e.Addr() == nil {
					b.Fatal("engine did not bind")
				}
				req := []byte("GET /bench HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
				one := func() error {
					c, err := net.Dial("tcp", addr)
					if err != nil {
						return err
					}
					defer func() { _ = c.Close() }()
					if arm == "bare" {
						return nil
					}
					if _, err := c.Write(req); err != nil {
						return err
					}
					_, err = io.Copy(io.Discard, c)
					return err
				}
				// Warm: the first connections after an idle ring wait for the worker's
				// idle wake.
				for range 2000 {
					if err := one(); err != nil {
						b.Fatalf("warm: %v", err)
					}
				}
				base := e.Metrics().CloseCount
				b.ReportAllocs()
				b.ResetTimer()
				var wg sync.WaitGroup
				next := make(chan struct{}, 256)
				for range 8 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for range next {
							if err := one(); err != nil {
								b.Errorf("conn: %v", err)
								return
							}
						}
					}()
				}
				for range b.N {
					next <- struct{}{}
				}
				close(next)
				wg.Wait()
				dl := time.Now().Add(60 * time.Second)
				for e.Metrics().CloseCount-base < uint64(b.N) {
					if time.Now().After(dl) {
						b.Fatalf("the engine closed %d of %d connections", e.Metrics().CloseCount-base, b.N)
					}
					time.Sleep(50 * time.Microsecond)
				}
				b.StopTimer()
			})
		}
	}
}
