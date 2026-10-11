//go:build linux

package epoll

// Benchmarks of celeris#930: what a flooding driver conn costs the other conns
// of its loop, and what the read budget costs the flooding conn and the paths
// that do not flood. They use no symbol the fix adds, so the same file runs on
// the base. Run with -benchtime=Nx; the metrics are:
//
//   - BenchmarkDriverFloodBystander930: one op is a one-byte ping of B, a
//     driver conn on the loop A floods; p50-ns/p99-ns/max-ns are B's latency,
//     unserved/op counts pings not served within 3 s (each counted as 3 s).
//   - BenchmarkHTTPBesideFlood930: the same with an HTTP/1 request on a
//     keep-alive conn of a loop that carries a flooding driver conn.
//   - BenchmarkDriverFloodThroughput930: one op is 1 MiB delivered to A's
//     onRecv while A's peer keeps the socket full; onRecv is empty (0s), sleeps
//     100 us a chunk, or reads every byte of the chunk (sum: a byte sum, crc: a
//     CRC-32).
//   - BenchmarkDriverPingPong930: one op is a one-byte write to an idle driver
//     conn and the loop's read of it (the path that never floods).

import (
	"bufio"
	"fmt"
	"net"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const b930Cap = 3 * time.Second

// b930Report reports the latency percentiles of a run and its unserved count.
func b930Report(b *testing.B, lat []time.Duration, unserved int) {
	b.Helper()
	if len(lat) == 0 {
		return
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pick := func(q float64) float64 { return float64(lat[int(q*float64(len(lat)-1))].Nanoseconds()) }
	b.ReportMetric(pick(0.5), "p50-ns")
	b.ReportMetric(pick(0.99), "p99-ns")
	b.ReportMetric(float64(lat[len(lat)-1].Nanoseconds()), "max-ns")
	b.ReportMetric(float64(unserved)/float64(len(lat)), "unserved/op")
}

func BenchmarkDriverFloodBystander930(b *testing.B) {
	for _, perChunk := range []time.Duration{100 * time.Microsecond, 0} {
		b.Run(fmt.Sprintf("onRecv=%v", perChunk), func(b *testing.B) {
			eng, stop := newTestEngine(b)
			defer stop()
			wl := eng.WorkerLoop(0)
			f := c930Flood(b, wl, perChunk)
			defer f.close()
			bfd, bPeer := c930Socketpair(b)
			defer func() { _ = wl.UnregisterConn(bfd); _ = unix.Close(bfd); _ = unix.Close(bPeer) }()
			bGot := make(chan struct{}, 1)
			if err := wl.RegisterConn(bfd, func([]byte) {
				select {
				case bGot <- struct{}{}:
				default:
				}
			}, func(error) {}); err != nil {
				b.Fatalf("RegisterConn(B): %v", err)
			}
			lat := make([]time.Duration, 0, b.N)
			unserved := 0
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				time.Sleep(time.Millisecond)
				select {
				case <-bGot:
				default:
				}
				t0 := time.Now()
				if _, err := unix.Write(bPeer, []byte{'b'}); err != nil {
					b.Fatalf("write B: %v", err)
				}
				select {
				case <-bGot:
					lat = append(lat, time.Since(t0))
				case <-time.After(b930Cap):
					lat = append(lat, b930Cap)
					unserved++
					select { // the byte is read once A's turn ends: let it land before the next ping
					case <-bGot:
					case <-time.After(10 * time.Second):
					}
				}
			}
			b.StopTimer()
			b930Report(b, lat, unserved)
		})
	}
}

func BenchmarkHTTPBesideFlood930(b *testing.B) {
	e := c930HTTPEngine(b)
	for i := 0; i < e.NumWorkers(); i++ {
		f := c930Flood(b, e.WorkerLoop(i), 100*time.Microsecond)
		defer f.close()
	}
	dial := func() (net.Conn, *bufio.Reader) {
		c, err := net.DialTimeout("tcp", e.Addr().String(), 2*time.Second)
		if err != nil {
			b.Fatalf("dial: %v", err)
		}
		return c, bufio.NewReader(c)
	}
	c, r := dial()
	defer func() { _ = c.Close() }()
	lat := make([]time.Duration, 0, b.N)
	unserved := 0
	req := []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		t0 := time.Now()
		_ = c.SetDeadline(t0.Add(b930Cap))
		ok := false
		if _, err := c.Write(req); err == nil {
			// status line, headers up to the blank line, then the 2-byte body
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					break
				}
				if line == "\r\n" {
					var body [2]byte
					_, err = r.Read(body[:1])
					if err == nil {
						_, err = r.Read(body[1:])
					}
					ok = err == nil
					break
				}
			}
		}
		if ok {
			lat = append(lat, time.Since(t0))
			continue
		}
		lat = append(lat, b930Cap)
		unserved++
		_ = c.Close()
		c, r = dial() // a conn that timed out may still have a response in flight
	}
	b.StopTimer()
	b930Report(b, lat, unserved)
}

func BenchmarkDriverFloodThroughput930(b *testing.B) {
	c930FeederSpin.Store(true)
	defer c930FeederSpin.Store(false)
	for _, tc := range []struct {
		name     string
		perChunk time.Duration
		touch    func([]byte)
	}{
		{fmt.Sprintf("onRecv=%v", time.Duration(0)), 0, nil},
		{fmt.Sprintf("onRecv=%v", 100*time.Microsecond), 100 * time.Microsecond, nil},
		{"onRecv=sum", 0, c930Sum},
		{"onRecv=crc", 0, c930CRC},
	} {
		b.Run(tc.name, func(b *testing.B) {
			eng, stop := newTestEngine(b)
			defer stop()
			f := c930FloodTouching(b, eng.WorkerLoop(0), tc.perChunk, nil, tc.touch)
			defer f.close()
			b.SetBytes(1 << 20)
			start := f.got.Load()
			t0 := time.Now()
			b.ResetTimer()
			target := start + int64(b.N)<<20
			for f.got.Load() < target {
				time.Sleep(200 * time.Microsecond)
			}
			b.StopTimer()
			b.ReportMetric(float64(f.got.Load()-start)/time.Since(t0).Seconds()/(1<<20), "MiB/s")
		})
	}
}

func BenchmarkDriverPingPong930(b *testing.B) {
	eng, stop := newTestEngine(b)
	defer stop()
	wl := eng.WorkerLoop(0)
	a, aPeer := c930Socketpair(b)
	defer func() { _ = wl.UnregisterConn(a); _ = unix.Close(a); _ = unix.Close(aPeer) }()
	var got atomic.Int64
	if err := wl.RegisterConn(a, func(p []byte) { got.Add(int64(len(p))) }, func(error) {}); err != nil {
		b.Fatalf("RegisterConn: %v", err)
	}
	one := []byte{'x'}
	b.ResetTimer()
	for i := 1; i <= b.N; i++ {
		if _, err := unix.Write(aPeer, one); err != nil {
			b.Fatalf("write: %v", err)
		}
		for got.Load() < int64(i) { // spin: the op is the loop's wake, read and callback
		}
	}
}
