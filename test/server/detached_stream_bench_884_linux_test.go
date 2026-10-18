//go:build linux

package celeris_test

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// BenchmarkDetachedStreamWriteFlush884 drives a detached HTTP/1 StreamWriter
// over a real engine, an SSE event as middleware/sse sends it: one Write, then
// one Flush, from the goroutine that outlived the handler (every Write goes
// through the engine's guarded write hook, every Write and Flush through the
// write-error query of celeris#884 item 45 / #885 item 30). conns streams run
// at once, each b.N events of 128 bytes to a client that reads them all. The
// reported ns/op is the producers' time per event: the longest producer's
// loop (from the first Write to the last Flush) over b.N, not the run's wall
// time, so that the client's reads and the setup are outside it. Run it with a
// fixed -benchtime=Nx. The file uses only the public API, so it compiles on
// main too (the A of an A/B).
func BenchmarkDetachedStreamWriteFlush884(b *testing.B) {
	for _, e := range []struct {
		name string
		eng  celeris.EngineType
	}{{"epoll", celeris.Epoll}, {"io_uring", celeris.IOUring}} {
		for _, conns := range []int{1, 16} {
			b.Run(fmt.Sprintf("%s/conns=%d", e.name, conns), func(b *testing.B) {
				events := b.N
				chunk := []byte(strings.Repeat("e", 128))
				elapsed := make(chan time.Duration, conns)
				ready := make(chan struct{}, conns)
				start := make(chan struct{})
				addr := startServerConfig761(b, celeris.Config{Engine: e.eng, Protocol: celeris.HTTP1}, func(s *celeris.Server) {
					s.GET("/ev884", func(c *celeris.Context) error {
						sw := c.StreamWriter()
						if sw == nil {
							return fmt.Errorf("no StreamWriter")
						}
						done := c.Detach()
						go func() {
							defer done()
							if err := sw.WriteHeader(200, [][2]string{{"content-type", "text/event-stream"}}); err != nil {
								elapsed <- -1
								return
							}
							ready <- struct{}{}
							<-start
							t0 := time.Now()
							for i := 0; i < events; i++ {
								if _, err := sw.Write(chunk); err != nil {
									elapsed <- -1
									return
								}
								if err := sw.Flush(); err != nil {
									elapsed <- -1
									return
								}
							}
							elapsed <- time.Since(t0)
							_ = sw.Close()
						}()
						return nil
					})
				})
				var wg sync.WaitGroup
				for i := 0; i < conns; i++ {
					c, err := net.DialTimeout("tcp", addr, 2*time.Second)
					if err != nil {
						b.Fatal(err)
					}
					if _, err := c.Write([]byte("GET /ev884 HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
						b.Fatal(err)
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						defer func() { _ = c.Close() }()
						// Read to the last chunk: the connection stays open after it.
						buf := make([]byte, 64<<10)
						var tail []byte
						for {
							n, err := c.Read(buf)
							if n > 0 {
								tail = append(tail, buf[:n]...)
								if len(tail) > 5 {
									tail = append(tail[:0], tail[len(tail)-5:]...)
								}
								if string(tail) == "0\r\n\r\n" {
									return
								}
							}
							if err != nil {
								return
							}
						}
					}()
				}
				for i := 0; i < conns; i++ {
					select {
					case <-ready:
					case <-time.After(20 * time.Second):
						b.Fatal("a stream did not start")
					}
				}
				b.ResetTimer()
				close(start)
				var worst time.Duration
				for i := 0; i < conns; i++ {
					select {
					case d := <-elapsed:
						if d < 0 {
							b.Fatal("a Write or Flush failed")
						}
						worst = max(worst, d)
					case <-time.After(5 * time.Minute):
						b.Fatal("the producers did not finish")
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(worst.Nanoseconds())/float64(events), "ns/op")
				wg.Wait()
			})
		}
	}
}
