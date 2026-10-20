//go:build linux

package epoll

import (
	"bufio"
	"context"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// Benchmarks for the recv path's two shapes (celeris#884 items 37 and 39,
// #885 items 16 and 17: handleRecv/drainRead's verdict handling moved into
// helpers). One op is one request and its response on a keep-alive
// connection; 4 connections per GOMAXPROCS drive 2 workers. "get" is the
// plain request the main block serves; "post" sends a 32000-byte body in two
// writes, so that the second recv is received straight into bodyBuf when it
// does not coalesce with the first (the direct-body block).
func BenchmarkEpollRecvShapes884(b *testing.B) {
	for _, shape := range []string{"get", "post"} {
		b.Run(shape, func(b *testing.B) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatalf("pick port: %v", err)
			}
			addr := ln.Addr().String()
			_ = ln.Close()
			eng, err := New(resource.Config{
				Addr:      addr,
				Protocol:  engine.HTTP1,
				Resources: resource.Resources{Workers: 2},
			}, newCarry543Handler())
			if err != nil {
				b.Fatalf("epoll engine unavailable: %v", err)
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
			const parallelism = 4
			conns := parallelism * runtime.GOMAXPROCS(0)
			head := "GET /bench HTTP/1.1\r\nHost: x\r\n\r\n"
			var first, second []byte
			if shape == "post" {
				body := strings.Repeat("z", 32000)
				h := "POST /bench HTTP/1.1\r\nHost: x\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
				first, second = []byte(h+body[:16000]), []byte(body[16000:])
			} else {
				first = []byte(head)
			}
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
			b.SetParallelism(parallelism)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				cl := <-clients
				defer func() { clients <- cl }()
				for pb.Next() {
					if _, err := cl.c.Write(first); err != nil {
						b.Errorf("write: %v", err)
						return
					}
					if second != nil {
						if _, err := cl.c.Write(second); err != nil {
							b.Errorf("write: %v", err)
							return
						}
					}
					if err := discardResponse(cl.br); err != nil {
						b.Errorf("read: %v", err)
						return
					}
				}
			})
		})
	}
}
