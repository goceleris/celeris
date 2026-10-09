//go:build linux

package epoll

import (
	"bufio"
	"context"
	"net"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// BenchmarkKeepAliveDispatch measures what celeris#771 adds to the HTTP
// path: the worker reads each event's Pad next to its Fd and, with no driver
// conn registered, nothing more (driverCandidate); with one registered, the
// lookup it always did plus a generation compare. One op is one request and
// response on a keep-alive connection; 4 connections per GOMAXPROCS drive 2 workers. The
// "withdriver" arm keeps an idle driver conn registered on every worker so
// that each HTTP event takes the lookup (celeris#771: before, lookupDriver;
// after, dispatchDriver).
func BenchmarkKeepAliveDispatch(b *testing.B) {
	for _, withDriver := range []bool{false, true} {
		name := "nodriver"
		if withDriver {
			name = "withdriver"
		}
		b.Run(name, func(b *testing.B) {
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
			}, &asyncReuseHandler{})
			if err != nil {
				b.Fatalf("New: %v", err)
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
			if withDriver {
				for w := 0; w < eng.NumWorkers(); w++ {
					wl := eng.WorkerLoop(w)
					fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
					if err != nil {
						b.Fatalf("socketpair: %v", err)
					}
					defer func() { _ = unix.Close(fds[0]); _ = unix.Close(fds[1]) }()
					if err := wl.RegisterConn(fds[0], func([]byte) {}, func(error) {}); err != nil {
						b.Fatalf("RegisterConn: %v", err)
					}
					defer func() { _ = wl.UnregisterConn(fds[0]) }()
				}
			}
			// RunParallel starts parallelism*GOMAXPROCS goroutines, one connection each.
			const parallelism = 4
			conns := parallelism * runtime.GOMAXPROCS(0)
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
			b.SetParallelism(parallelism)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				cl := <-clients
				defer func() { clients <- cl }()
				for pb.Next() {
					if _, err := cl.c.Write(req); err != nil {
						b.Errorf("write: %v", err)
						return
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
