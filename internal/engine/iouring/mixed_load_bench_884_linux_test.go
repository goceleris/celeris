//go:build linux

package iouring

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// mixedHandler884 answers /big with a large body and anything else with "ok".
type mixedHandler884 struct {
	big   []byte
	async bool
}

func (h *mixedHandler884) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := []byte("ok")
	if s.Path == "/big" {
		body = h.big
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "application/octet-stream"}, {"content-length", strconv.Itoa(len(body))}}, body)
}
func (h *mixedHandler884) RouteAsync(_, _ string) bool { return h.async }
func (h *mixedHandler884) HasAsyncRoutes() bool        { return h.async }

// BenchmarkMixedLargeSmall884 is the load celeris#884 item 62's change is
// measured on: two workers, six clients whose 256 KiB-window readers take
// 2 MiB responses back to back (each response's tail is a remainder SEND, so
// the worker is out of its untimed wait, mode 3a, while one is outstanding),
// and the benchmark's own keep-alive client making small requests, whose
// round trip is ns/op (the path that wants mode 3a; it shares a worker with
// some of the large clients, which of them is the kernel's choice per run). big_MBps is the large
// clients' goodput over the run. The A/B method is an interleaved base/fix/
// base2 A/B, base2 an A/A copy of the base, which gives the floor.
func BenchmarkMixedLargeSmall884(b *testing.B) {
	for _, async := range []bool{false, true} {
		mode := "sync"
		if async {
			mode = "async"
		}
		b.Run(mode, func(b *testing.B) {
			big := make([]byte, 2<<20)
			for i := range big {
				big[i] = byte(i)
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatalf("pick port: %v", err)
			}
			addr := ln.Addr().String()
			_ = ln.Close()
			e, err := New(resource.Config{
				Addr:          addr,
				Protocol:      engine.HTTP1,
				Resources:     resource.Resources{Workers: 2, SocketSend: 64 << 10},
				AsyncHandlers: async,
				Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
			}, &mixedHandler884{big: big, async: async})
			if err != nil {
				b.Skipf("iouring engine unavailable: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- e.Listen(ctx) }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					b.Error("engine did not stop within 5s")
				}
			}()
			dial := func(rcvbuf int) net.Conn {
				d := net.Dialer{Timeout: 3 * time.Second, Control: func(_, _ string, rc syscall.RawConn) error {
					return rc.Control(func(fd uintptr) {
						if rcvbuf > 0 {
							_ = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, rcvbuf)
						}
					})
				}}
				var c net.Conn
				for deadline := time.Now().Add(8 * time.Second); ; {
					if c, err = d.Dial("tcp", addr); err == nil && e.NumWorkers() > 0 {
						return c
					}
					if c != nil {
						_ = c.Close()
					}
					if time.Now().After(deadline) {
						b.Fatal("engine did not start listening within 8s")
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			var stop atomic.Bool
			var bigBytes atomic.Int64
			var wg sync.WaitGroup
			for range 6 {
				c := dial(256 << 10)
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { _ = c.Close() }()
					br := bufio.NewReaderSize(c, 256<<10)
					req := []byte("GET /big HTTP/1.1\r\nHost: x\r\n\r\n")
					for !stop.Load() {
						_ = c.SetDeadline(time.Now().Add(10 * time.Second))
						if _, err := c.Write(req); err != nil {
							return
						}
						n, err := rawResponseLen(br)
						if err != nil {
							return
						}
						bigBytes.Add(int64(n))
					}
				}()
			}
			c := dial(0)
			defer func() { _ = c.Close() }()
			br := bufio.NewReaderSize(c, 4<<10)
			req := []byte("GET /s HTTP/1.1\r\nHost: x\r\n\r\n")
			read := func() {
				if _, err := c.Write(req); err != nil {
					b.Fatal(err)
				}
				if _, err := rawResponseLen(br); err != nil {
					b.Fatal(err)
				}
			}
			for range 200 {
				read()
			}
			b.ReportAllocs()
			bigBytes.Store(0)
			start := time.Now()
			b.ResetTimer()
			for b.Loop() {
				read()
			}
			b.StopTimer()
			b.ReportMetric(float64(bigBytes.Load())/time.Since(start).Seconds()/1e6, "big_MBps")
			stop.Store(true)
			wg.Wait()
		})
	}
}
