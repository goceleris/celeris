//go:build linux

package epoll

// celeris#885 item 41: drainRead reads an HTTP conn until a read returns less
// than the buffer holds, with no bound on the number of reads, so a client
// that keeps its socket full holds the loop, and the conns on it wait.
//
// Two measurements. TestDrainReadIsBoundedUnderAFlood885 is the gate: one
// drainRead call on a conn whose peer never lets the socket empty must return
// after a bounded number of bytes (deterministic: it counts bytes, not time).
// TestProbeDrainReadFloodLatency885 is the end-to-end probe behind it, run by
// hand (CELERIS_PROBE_885=1): conn A pipelines requests as fast as its socket
// takes them, conn B on A's loop sends one request every few milliseconds and
// times the answer, and conn C on the other loop is the control (a stall that
// C shares is CPU contention of the test process, not the loop).

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// floodHandler885 answers every request with 200; /who carries the id of the
// worker (loop) that owns the conn in its body.
type floodHandler885 struct{}

func (floodHandler885) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	body := []byte("ok")
	if s.Path == "/who" {
		body = []byte(strconv.Itoa(int(s.WorkerID)))
	}
	return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, body)
}

func startFloodEngine885(t testing.TB, bufSize int) *Engine {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg := resource.Config{
		Addr:     addr,
		Protocol: engine.HTTP1,
		Resources: resource.Resources{
			Workers:    resource.MinWorkers,
			BufferSize: bufSize,
		},
	}
	e, err := New(cfg, floodHandler885{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() { errCh <- e.Listen(ctx) }()
	t.Cleanup(func() { cancel(); <-errCh })
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl) && e.Addr() == nil; {
		time.Sleep(5 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("engine did not bind")
	}
	return e
}

// pingConn885 is a client conn that asks /who and times requests.
type pingConn885 struct {
	c  net.Conn
	br *bufio.Reader
}

func (p *pingConn885) ask(path string) (string, time.Duration, error) {
	start := time.Now()
	_ = p.c.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := p.c.Write([]byte("GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		return "", 0, err
	}
	resp, err := http.ReadResponse(p.br, nil)
	if err != nil {
		return "", 0, err
	}
	var sb strings.Builder
	buf := make([]byte, 64)
	for {
		n, rerr := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	_ = resp.Body.Close()
	return sb.String(), time.Since(start), nil
}

// threeConns885 dials until it holds conn a, conn same on a's loop and conn
// other on the other loop (the engine has two), and closes the rest.
func threeConns885(t testing.TB, e *Engine) (a, same, other *pingConn885) {
	t.Helper()
	byLoop := map[string][]*pingConn885{}
	var all []*pingConn885
	for i := 0; i < 64; i++ {
		c, err := net.DialTimeout("tcp", e.Addr().String(), 3*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		p := &pingConn885{c: c, br: bufio.NewReader(c)}
		all = append(all, p)
		who, _, err := p.ask("/who")
		if err != nil {
			t.Fatalf("who: %v", err)
		}
		byLoop[who] = append(byLoop[who], p)
		if len(byLoop) < 2 {
			continue
		}
		for _, v := range byLoop {
			if len(v) >= 2 {
				a, same = v[0], v[1]
			} else {
				other = v[0]
			}
		}
		if a != nil && other != nil {
			break
		}
	}
	if a == nil || other == nil {
		t.Fatal("did not get two conns on one loop and one on the other in 64 dials")
	}
	for _, p := range all {
		if p != a && p != same && p != other {
			_ = p.c.Close()
		}
	}
	t.Cleanup(func() { _ = a.c.Close(); _ = same.c.Close(); _ = other.c.Close() })
	return a, same, other
}

type floodStats885 struct {
	n             int
	p50, p99, max time.Duration
}

func (s floodStats885) String() string {
	return fmt.Sprintf("n=%d p50=%v p99=%v max=%v", s.n, s.p50, s.p99, s.max)
}

func summarize885(lat []time.Duration) floodStats885 {
	if len(lat) == 0 {
		return floodStats885{}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	return floodStats885{n: len(lat), p50: lat[len(lat)/2], p99: lat[len(lat)*99/100], max: lat[len(lat)-1]}
}

// runFlood885 floods a (when flood is set) for dur while each of the pingers
// asks /who every gap, and returns each one's latencies, and how many
// requests the flood sent.
func runFlood885(t testing.TB, a *pingConn885, pingers []*pingConn885, flood bool, dur, gap time.Duration) ([]floodStats885, int64) {
	t.Helper()
	var stop atomic.Bool
	var wg sync.WaitGroup
	var sent atomic.Int64
	if flood {
		req := "GET /flood HTTP/1.1\r\nHost: x\r\n\r\n"
		chunk := []byte(strings.Repeat(req, (64<<10)/len(req)))
		per := int64(len(chunk) / len(req))
		wg.Add(2)
		go func() { // reader: discard the responses
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
		go func() { // writer: as fast as the socket takes it
			defer wg.Done()
			for !stop.Load() {
				_ = a.c.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
				n, err := a.c.Write(chunk)
				sent.Add(per * int64(n) / int64(len(chunk)))
				if err != nil {
					if ne, ok := err.(net.Error); ok && ne.Timeout() {
						continue
					}
					return
				}
			}
		}()
		time.Sleep(50 * time.Millisecond) // let the flood build up
	}
	lats := make([][]time.Duration, len(pingers))
	var pw sync.WaitGroup
	end := time.Now().Add(dur)
	for i, p := range pingers {
		pw.Add(1)
		go func() {
			defer pw.Done()
			for time.Now().Before(end) {
				_, d, err := p.ask("/who")
				if err != nil {
					t.Errorf("ping %d: %v", i, err)
					return
				}
				lats[i] = append(lats[i], d)
				time.Sleep(gap)
			}
		}()
	}
	pw.Wait()
	stop.Store(true)
	wg.Wait()
	out := make([]floodStats885, len(pingers))
	for i := range pingers {
		out[i] = summarize885(lats[i])
	}
	return out, sent.Load()
}

// TestProbeDrainReadFloodLatency885 is the probe, not a gate: it logs the
// latency of a conn on the flooded conn's loop and of one on the other loop,
// with and without the flood.
func TestProbeDrainReadFloodLatency885(t *testing.T) {
	if os.Getenv("CELERIS_PROBE_885") == "" {
		t.Skip("a measurement, not a gate: CELERIS_PROBE_885=1 runs it (about 10 s per buffer size)")
	}
	for _, buf := range []int{0, 65536} {
		t.Run(fmt.Sprintf("buf%d", buf), func(t *testing.T) {
			e := startFloodEngine885(t, buf)
			a, same, other := threeConns885(t, e)
			base, _ := runFlood885(t, a, []*pingConn885{same, other}, false, 2*time.Second, 5*time.Millisecond)
			t.Logf("no flood      same-loop B: %v | other-loop C: %v", base[0], base[1])
			fl, sent := runFlood885(t, a, []*pingConn885{same, other}, true, 3*time.Second, 5*time.Millisecond)
			t.Logf("under flood   same-loop B: %v | other-loop C: %v | flood requests sent=%d", fl[0], fl[1], sent)
		})
	}
}

// floodFeed885 writes pipelined "GET /flood" requests to peer as fast as its
// socket takes them until stop, keeping the stream request-aligned across
// partial writes. queued counts the bytes the socket took.
func floodFeed885(peer int, stop *atomic.Bool, queued *atomic.Int64) {
	req := "GET /flood HTTP/1.1\r\nHost: x\r\n\r\n"
	pattern := []byte(strings.Repeat(req, (64<<10)/len(req)+2))
	var total int64
	for !stop.Load() {
		off := int(total % int64(len(req)))
		n, err := unix.Write(peer, pattern[off:off+(64<<10)])
		if n > 0 {
			total += int64(n)
			queued.Store(total)
		}
		if err != nil {
			time.Sleep(20 * time.Microsecond)
		}
	}
}

// TestDrainReadIsBoundedUnderAFlood885 is celeris#885 item 41 as an assertion:
// the peer keeps the conn's socket full (its queue full before the read, more
// written while it goes on, and the loop parses far slower than the
// peer writes), and one drainRead call, which is one readiness event, must
// return after reading a bounded number of bytes instead of when the peer
// pauses. A watchdog stops the peer after 2 s so that an unbounded drainRead
// ends the test instead of hanging it.
func TestDrainReadIsBoundedUnderAFlood885(t *testing.T) {
	l := newLedgerLoop(t)
	l.handler = floodHandler885{}
	fd, peer, cs := adoptedPair885(t, l)
	var b [8]byte
	_, _ = unix.Read(fd, b[:]) // the byte adoptedPair885 wrote to have the ADD report EPOLLIN
	// A bigger queue where the host allows one (net.core.wmem_max): the more is
	// queued before the read, the longer the peer can stall without the socket
	// emptying. At the default cap the queue is still about 210 KB, three times
	// the budget.
	for _, s := range []int{fd, peer} {
		_ = unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_SNDBUF, 4<<20)
	}

	var stop atomic.Bool
	var queued atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); floodFeed885(peer, &stop, &queued) }()
	go func() { // the peer takes the responses
		defer wg.Done()
		buf := make([]byte, 256<<10)
		for !stop.Load() {
			if _, err := unix.Read(peer, buf); err != nil {
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()
	t.Cleanup(func() { stop.Store(true); wg.Wait() })

	const preload = 128 << 10
	for dl := time.Now().Add(5 * time.Second); queued.Load() < preload; time.Sleep(time.Millisecond) {
		if time.Now().After(dl) {
			t.Fatalf("apparatus: the peer queued only %d bytes in 5 s", queued.Load())
		}
	}
	watchdog := time.AfterFunc(2*time.Second, func() { stop.Store(true) })
	defer watchdog.Stop()

	before := l.bytesReadBatch
	start := time.Now()
	l.drainRead(fd, time.Now().UnixNano())
	took := time.Since(start)
	read := l.bytesReadBatch - before
	fedDuring := queued.Load()
	stoppedByWatchdog := stop.Load()

	// The bound is far above any sensible budget (a few hundred KiB) and far
	// below what an unbounded read takes in 2 s.
	const bound = 2 << 20
	if read > bound || stoppedByWatchdog {
		t.Fatalf("celeris#885 item 41: one drainRead read %d bytes in %v (the peer had written %d) and returned only when the peer stopped (watchdog fired: %v); "+
			"a conn whose peer keeps its socket full holds the loop for as long as it does, and the other conns on the loop wait",
			read, took, fedDuring, stoppedByWatchdog)
	}
	if l.conns[fd] != cs {
		t.Fatalf("the conn was closed by the read (read %d bytes)", read)
	}
}
