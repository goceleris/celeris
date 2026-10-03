//go:build linux

package eventloop

// The cost of RegisterConn's EPOLL_CTL_ADD on the other conns of the worker
// (celeris#862 issues it under w.mu's read lock, after the conn is in the
// map; the base issued it with no lock). Conn R's round trip through a
// running worker (R's peer writes one byte, R's onRecv signals) while
// `churn` goroutines register and unregister fresh socketpair ends on the
// same worker as fast as they can, the shape of a pool closing and dialing
// conns. Every event the worker dispatches takes w.mu's read lock, and every
// registration and teardown takes its write lock once. Reports R's mean
// (sec/op), p50/p99, and the churners' register+unregister pairs per second.
// It uses only the WorkerLoop API, so the same file runs on the base.

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func BenchmarkRegisterChurn862(b *testing.B) {
	for _, churn := range []int{0, 1, 4} {
		b.Run(fmt.Sprintf("churn=%d", churn), func(b *testing.B) {
			l, err := New(1)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = l.Close() }()
			w := l.WorkerLoop(0)

			var stop atomic.Bool
			var pairs atomic.Int64
			var wg sync.WaitGroup
			for range churn {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for !stop.Load() {
						fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
						if err != nil {
							return
						}
						if err := w.RegisterConn(fds[0], func([]byte) {}, func(error) {}); err == nil {
							_ = w.UnregisterConn(fds[0])
							pairs.Add(1)
						}
						_ = unix.Close(fds[0])
						_ = unix.Close(fds[1])
					}
				}()
			}

			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				b.Fatal(err)
			}
			if err := unix.SetNonblock(fds[0], true); err != nil {
				b.Fatal(err)
			}
			r, rPeer := fds[0], fds[1]
			got := make(chan struct{}, 1)
			if err := w.RegisterConn(r, func([]byte) { got <- struct{}{} }, func(error) {}); err != nil {
				b.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
			lat := make([]time.Duration, 0, 1<<16)
			one := []byte{'r'}
			p0 := pairs.Load()
			t0 := time.Now()
			for b.Loop() {
				s := time.Now()
				if _, err := unix.Write(rPeer, one); err != nil {
					b.Fatal(err)
				}
				select {
				case <-got:
				case <-time.After(10 * time.Second):
					b.Fatal("R not served in 10 s")
				}
				lat = append(lat, time.Since(s))
			}
			el := time.Since(t0)
			p1 := pairs.Load()
			stop.Store(true)
			wg.Wait()
			_ = w.UnregisterConn(r)
			_ = unix.Close(r)
			_ = unix.Close(rPeer)
			sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
			q := func(p float64) float64 {
				if len(lat) == 0 {
					return 0
				}
				return float64(lat[int(p*float64(len(lat)-1))].Nanoseconds())
			}
			b.ReportMetric(q(0.50), "p50-ns")
			b.ReportMetric(q(0.99), "p99-ns")
			b.ReportMetric(float64(p1-p0)/el.Seconds(), "pairs/s")
		})
	}
}
