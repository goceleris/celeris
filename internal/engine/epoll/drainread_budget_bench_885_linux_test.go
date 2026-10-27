//go:build linux

package epoll

// Benchmarks of the read budget (celeris#885 item 41), for an A/B of drainRead
// with and without it. The ping-pong shapes (one short read per request: the
// budget costs an add and a compare) are BenchmarkEpollRecvShapes884 and
// BenchmarkKeepAliveDispatch; these are the two shapes that reach the budget:
//
//   - BenchmarkDrainReadFloodThroughput885: one conn, the loop otherwise idle,
//     so a yield costs a MOD and an epoll_wait and buys nothing. "pipeline" is
//     pipelined GETs written 64 KiB at a time; "upload" is a 4 MiB POST body (just under the default body limit), a
//     read straight into the body buffer. One op is one write (or one POST).
//   - BenchmarkDrainReadFloodBystander885: the conn floods and a second conn on
//     the same loop does ping-pong; one op is one round trip of the second, and
//     the latency it sees while the first floods is reported as p99 and max.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func BenchmarkDrainReadFloodThroughput885(b *testing.B) {
	for _, shape := range []string{"pipeline", "upload"} {
		b.Run(shape, func(b *testing.B) {
			e := startFloodEngine885(b, 0)
			c, err := net.DialTimeout("tcp", e.Addr().String(), 3*time.Second)
			if err != nil {
				b.Fatalf("dial: %v", err)
			}
			defer func() { _ = c.Close() }()
			cr := &countReader885{r: c}
			br := bufio.NewReaderSize(cr, 256<<10)
			var chunk, onePost []byte
			var perOp int
			if shape == "pipeline" {
				req := "GET /flood HTTP/1.1\r\nHost: x\r\n\r\n"
				perOp = (64 << 10) / len(req)
				chunk = []byte(strings.Repeat(req, perOp))
				onePost = []byte(req)
				b.SetBytes(int64(len(chunk)))
			} else {
				body := strings.Repeat("u", 4<<20)
				chunk = []byte("POST /flood HTTP/1.1\r\nHost: x\r\nContent-Length: " + fmt.Sprint(len(body)) + "\r\n\r\n" + body)
				onePost = chunk
				perOp = 1
				b.SetBytes(int64(len(chunk)))
			}
			// One response, to learn its size: the flood's responses are then
			// counted in bytes, which is all the reader can keep up with.
			if _, err := c.Write(onePost); err != nil {
				b.Fatalf("probe write: %v", err)
			}
			r, err := http.ReadResponse(br, nil)
			if err != nil {
				b.Fatalf("probe: %v", err)
			}
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
			if r.StatusCode != 200 {
				b.Fatalf("probe: status %d", r.StatusCode)
			}
			respLen := cr.n.Load() - int64(br.Buffered())
			var got atomic.Int64
			done := make(chan struct{})
			go func() { // count the response bytes
				defer close(done)
				buf := make([]byte, 256<<10)
				got.Add(int64(br.Buffered()))
				_, _ = br.Discard(br.Buffered())
				for {
					n, err := c.Read(buf)
					got.Add(int64(n))
					if err != nil {
						return
					}
				}
			}()
			// At most window ops are unanswered: a peer that lets its
			// responses pile up past the write cap is closed, by design, and
			// that is not what is measured here.
			window := int64(8)
			if shape == "upload" {
				window = 2
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for dl := time.Now().Add(60 * time.Second); got.Load() < (int64(i)-window)*int64(perOp)*respLen; time.Sleep(20 * time.Microsecond) {
					if time.Now().After(dl) {
						b.Fatalf("op %d: the server answered %d bytes", i, got.Load())
					}
				}
				if _, err := c.Write(chunk); err != nil {
					b.Fatalf("write: %v", err)
				}
			}
			want := int64(b.N*perOp) * respLen
			for dl := time.Now().Add(60 * time.Second); got.Load() < want; time.Sleep(50 * time.Microsecond) {
				if time.Now().After(dl) {
					b.Fatalf("got %d of %d response bytes", got.Load(), want)
				}
			}
			b.StopTimer()
			_ = c.Close()
			<-done
		})
	}
}

func BenchmarkDrainReadFloodBystander885(b *testing.B) {
	e := startFloodEngine885(b, 0)
	a, same, _ := threeConns885(b, e)
	var stop atomic.Bool
	var wg sync.WaitGroup
	req := "GET /flood HTTP/1.1\r\nHost: x\r\n\r\n"
	chunk := []byte(strings.Repeat(req, (64<<10)/len(req)))
	wg.Add(2)
	go func() {
		defer wg.Done()
		buf := make([]byte, 256<<10)
		for !stop.Load() {
			_ = a.c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			if _, err := a.c.Read(buf); err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_ = a.c.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
			if _, err := a.c.Write(chunk); err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
		}
	}()
	// Stops the flood on every way out, a Fatalf too: a leaked flood goes on
	// loading the process for the benchmarks that follow.
	defer func() {
		stop.Store(true)
		_ = a.c.Close()
		wg.Wait()
	}()
	time.Sleep(100 * time.Millisecond)
	lat := make([]time.Duration, 0, b.N)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, d, err := same.ask("/who")
		if err != nil {
			b.Fatalf("ask: %v", err)
		}
		lat = append(lat, d)
	}
	b.StopTimer()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	b.ReportMetric(float64(lat[len(lat)*99/100]), "p99-ns")
	b.ReportMetric(float64(lat[len(lat)-1]), "max-ns")
}

// countReader885 counts the bytes read through it.
type countReader885 struct {
	r io.Reader
	n atomic.Int64
}

func (c *countReader885) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// BenchmarkDrainReadLoop885 is drainRead alone: a conn of the loop's own making
// over a socketpair, no engine, no client scheduler. One op writes n requests
// from the peer, has the loop read them (one drainRead call: the read, ProcessH1
// and the handler, the inline flush) and the peer takes the responses. The cost
// of the read budget on this path is an add and a compare per read, and of
// flushInline, one call per read (a call per recv, not per request).
func BenchmarkDrainReadLoop885(b *testing.B) {
	for _, n := range []int{1, 16} {
		b.Run(fmt.Sprintf("reqs=%d", n), func(b *testing.B) {
			l := newLedgerLoop(b)
			l.handler = floodHandler885{}
			fd, peer, _ := adoptedPair885(b, l)
			var one [8]byte
			_, _ = unix.Read(fd, one[:]) // the byte adoptedPair885 wrote
			req := []byte(strings.Repeat("GET /flood HTTP/1.1\r\nHost: x\r\n\r\n", n))
			out := make([]byte, 64<<10)
			now := time.Now().UnixNano()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := unix.Write(peer, req); err != nil {
					b.Fatalf("write: %v", err)
				}
				l.drainRead(fd, now)
				if _, err := unix.Read(peer, out); err != nil {
					b.Fatalf("read: %v", err)
				}
			}
		})
	}
}
