//go:build linux

package epoll

// Helpers of the celeris#930 tests and benchmarks (driver_read_budget_930_linux_test.go,
// driver_read_bench_930_linux_test.go). They use no symbol the fix adds, so the
// benchmarks build on the base too.

import (
	"context"
	"hash/crc32"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// c930FeederSpin makes the flood's feeder spin on EAGAIN instead of sleeping
// 10 us, which a timer in a VM stretches into a bubble of the feeder's
// pipeline. The throughput benchmark sets it, so that it measures the loop and
// not the feeder's back-off.
var c930FeederSpin atomic.Bool

// c930TouchSink keeps the result of the touching onRecv functions alive.
var c930TouchSink atomic.Uint64

// c930Sum reads every byte of p: a byte sum.
func c930Sum(p []byte) {
	var s uint64
	for _, c := range p {
		s += uint64(c)
	}
	c930TouchSink.Add(s)
}

// c930CRC reads every byte of p: a CRC-32 (hardware-assisted, the cheapest per byte of the two).
func c930CRC(p []byte) {
	c930TouchSink.Add(uint64(crc32.ChecksumIEEE(p)))
}

// c930Socketpair is socketpairNonblocking for tests and benchmarks.
func c930Socketpair(tb testing.TB) (int, int) {
	tb.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		tb.Fatalf("socketpair: %v", err)
	}
	for _, fd := range fds {
		if err := unix.SetNonblock(fd, true); err != nil {
			tb.Fatalf("setnonblock: %v", err)
		}
	}
	return fds[0], fds[1]
}

// c930Pipe returns a non-blocking pipe (read end, write end) sized to 1 MiB.
func c930Pipe(t testing.TB) (int, int) {
	t.Helper()
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		t.Fatalf("pipe2: %v", err)
	}
	if _, err := unix.FcntlInt(uintptr(p[1]), unix.F_SETPIPE_SZ, 1<<20); err != nil {
		t.Fatalf("F_SETPIPE_SZ: %v", err)
	}
	return p[0], p[1]
}

// c930Pattern is n bytes whose byte i is byte((i+seed)%251).
func c930Pattern(seed, n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte((i + seed) % 251)
	}
	return p
}

// c930Sink receives one conn's bytes, which carry the pattern
// byte((offset+seed)%251), and records the first one out of order and the
// number of bytes it had when onClose fired.
type c930Sink struct {
	mu                    sync.Mutex
	seed, got, bad, reads int
	atClose, closes       int
	want                  int
	all                   chan struct{}
	closed                chan error
}

func c930NewSink(seed, want int) *c930Sink {
	return &c930Sink{seed: seed, want: want, bad: -1, atClose: -1, all: make(chan struct{}), closed: make(chan error, 2)}
}

func (s *c930Sink) onRecv(b []byte) {
	s.mu.Lock()
	s.reads++
	for i, c := range b {
		if s.bad < 0 && c != byte((s.got+i+s.seed)%251) {
			s.bad = s.got + i
		}
	}
	s.got += len(b)
	if s.got == s.want {
		close(s.all)
	}
	s.mu.Unlock()
}

func (s *c930Sink) onClose(err error) {
	s.mu.Lock()
	s.closes++
	s.atClose = s.got
	s.mu.Unlock()
	s.closed <- err
}

// c930Engine starts a two-worker epoll engine that answers every HTTP/1
// request with "ok", for the tests that put an HTTP conn on a loop with a
// flooding driver conn.
type c930Resp struct{}

func (c930Resp) HandleStream(_ context.Context, s *stream.Stream) error {
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}

func c930HTTPEngine(t testing.TB) *Engine {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
		Logger:    slog.New(slog.DiscardHandler),
	}, c930Resp{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("engine did not stop within 10 s")
		}
	})
	for dl := time.Now().Add(5 * time.Second); (e.Addr() == nil || e.NumWorkers() < 2) && time.Now().Before(dl); {
		time.Sleep(2 * time.Millisecond)
	}
	if e.Addr() == nil || e.NumWorkers() < 2 {
		t.Fatal("engine never bound")
	}
	return e
}

// c930Flooder floods one driver conn registered on wl: its peer writes 64 KiB
// chunks as fast as the socket takes them and its onRecv sleeps perChunk per
// chunk, so the socket never empties. close stops it and returns the bytes fed
// and delivered and how long the feeder ran.
type c930Flooder struct {
	fed, got  atomic.Int64
	stop      chan struct{}
	wg        sync.WaitGroup
	fd, peer  int
	start     time.Time
	unregOnce sync.Once
	wl        engine.WorkerLoop
}

func c930Flood(t testing.TB, wl engine.WorkerLoop, perChunk time.Duration) *c930Flooder {
	t.Helper()
	return c930FloodCounting(t, wl, perChunk, nil)
}

// c930FloodCounting is c930Flood that also counts the reads of the flooded conn in reads.
func c930FloodCounting(t testing.TB, wl engine.WorkerLoop, perChunk time.Duration, reads *atomic.Int64) *c930Flooder {
	t.Helper()
	return c930FloodTouching(t, wl, perChunk, reads, nil)
}

// c930FloodTouching is c930FloodCounting whose onRecv also runs touch on every chunk it receives, as an onRecv that
// reads the bytes (a parser, a checksum) does.
func c930FloodTouching(t testing.TB, wl engine.WorkerLoop, perChunk time.Duration, reads *atomic.Int64, touch func([]byte)) *c930Flooder {
	t.Helper()
	f := &c930Flooder{stop: make(chan struct{}), wl: wl}
	f.fd, f.peer = c930Socketpair(t)
	first := make(chan struct{}, 1)
	if err := wl.RegisterConn(f.fd, func(p []byte) {
		if reads != nil {
			reads.Add(1)
		}
		if f.got.Add(int64(len(p))) == int64(len(p)) {
			first <- struct{}{}
		}
		if touch != nil {
			touch(p)
		}
		if perChunk > 0 {
			time.Sleep(perChunk)
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn(flood): %v", err)
	}
	f.start = time.Now()
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		chunk := make([]byte, 64<<10)
		for {
			select {
			case <-f.stop:
				return
			default:
			}
			n, err := unix.Write(f.peer, chunk)
			if n > 0 {
				f.fed.Add(int64(n))
			}
			if err == unix.EAGAIN {
				if c930FeederSpin.Load() {
					runtime.Gosched()
				} else {
					time.Sleep(10 * time.Microsecond)
				}
				continue
			}
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("the flooded conn's onRecv never ran")
	}
	return f
}

func (f *c930Flooder) close() {
	f.unregOnce.Do(func() {
		close(f.stop)
		f.wg.Wait()
		_ = f.wl.UnregisterConn(f.fd)
		_ = unix.Close(f.fd)
		_ = unix.Close(f.peer)
	})
}
