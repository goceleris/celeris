//go:build linux

package eventloop

// The contended side of celeris#784's read lock (from the review of #843). A
// read of a driver conn must not wait for a flush of that conn: flushLocked
// holds c.mu across its whole write(2) loop, and the worker goroutine reads for
// every conn on it. This benchmark measures conn B's round trip through a
// worker (B's peer writes one byte, B's onRecv signals) while conn A on the
// same worker carries a pipelined echo load: a writer goroutine Writes chunk
// bytes to A as fast as the 4 MiB cap allows (pausing gap between Writes), and
// the server end echoes them, so A is readable while its writer is flushing.
// chunk=0 is the floor: no A at all. It runs the same code on the base and on
// the fix, so benchstat compares the two directly.
//
// Metrics: sec/op (mean B round trip), p50/p99/p999 of B's round trip, and A's
// echoed MB/s.

import (
	"fmt"
	"io"
	"net"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
)

// bench784EchoTCP returns a non-blocking fd of a loopback TCP conn whose
// server end echoes everything back, and a cleanup.
func bench784EchoTCP(b *testing.B) (int, func()) {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(c, c)
		_ = c.Close()
	}()
	cc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	f, err := cc.(*net.TCPConn).File()
	if err != nil {
		b.Fatal(err)
	}
	fd, err := dupFD(int(f.Fd()))
	if err != nil {
		b.Fatal(err)
	}
	return fd, func() {
		_ = f.Close()
		_ = cc.Close()
		_ = ln.Close()
		<-done
	}
}

func BenchmarkReadWhileAnotherConnFlushes784(b *testing.B) {
	for _, tc := range []struct {
		chunk int
		gap   time.Duration
	}{{0, 0}, {4 << 10, 0}, {64 << 10, 0}, {512 << 10, 0}, {256 << 10, time.Millisecond}, {1 << 20, 5 * time.Millisecond}} {
		chunk, gap := tc.chunk, tc.gap
		b.Run(fmt.Sprintf("chunk=%d/gap=%v", chunk, gap), func(b *testing.B) {
			l, err := New(1)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = l.Close() }()
			w := l.WorkerLoop(0)

			var echoed atomic.Int64
			var stop atomic.Bool
			var wg sync.WaitGroup
			var cleanupA func()
			afd := -1
			if chunk > 0 {
				afd, cleanupA = bench784EchoTCP(b)
				if err := w.RegisterConn(afd, func(p []byte) { echoed.Add(int64(len(p))) }, func(error) {}); err != nil {
					b.Fatal(err)
				}
				buf := make([]byte, chunk)
				wg.Add(1)
				go func() {
					defer wg.Done()
					for !stop.Load() {
						err := w.Write(afd, buf)
						if err == engine.ErrQueueFull {
							runtime.Gosched()
							continue
						}
						if err != nil {
							return
						}
						if gap > 0 {
							time.Sleep(gap)
						}
					}
				}()
			}

			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				b.Fatal(err)
			}
			if err := unix.SetNonblock(fds[0], true); err != nil {
				b.Fatal(err)
			}
			bfd, bPeer := fds[0], fds[1]
			got := make(chan struct{}, 1)
			if err := w.RegisterConn(bfd, func([]byte) { got <- struct{}{} }, func(error) {}); err != nil {
				b.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond) // the load reaches a steady state
			lat := make([]time.Duration, 0, 1<<16)
			one := []byte{'b'}
			e0 := echoed.Load()
			t0 := time.Now()
			b.ResetTimer()
			for b.Loop() {
				s := time.Now()
				if _, err := unix.Write(bPeer, one); err != nil {
					b.Fatal(err)
				}
				select {
				case <-got:
				case <-time.After(10 * time.Second):
					b.Fatal("B not served in 10 s")
				}
				lat = append(lat, time.Since(s))
			}
			b.StopTimer()
			el := time.Since(t0)
			e1 := echoed.Load()
			stop.Store(true)
			_ = w.UnregisterConn(bfd)
			_ = unix.Close(bfd)
			_ = unix.Close(bPeer)
			if afd >= 0 {
				_ = w.UnregisterConn(afd)
				_ = unix.Close(afd)
				cleanupA()
			}
			wg.Wait()
			sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
			q := func(p float64) float64 {
				if len(lat) == 0 {
					return 0
				}
				return float64(lat[int(p*float64(len(lat)-1))].Nanoseconds())
			}
			b.ReportMetric(q(0.50), "p50-ns")
			b.ReportMetric(q(0.99), "p99-ns")
			b.ReportMetric(q(0.999), "p999-ns")
			b.ReportMetric(float64(e1-e0)/el.Seconds()/1e6, "echoMB/s")
		})
	}
}
