//go:build linux

package epoll

// BENCH (lane EPOLL-HIJACK, celeris#710), evidence only. This file lives on the
// measure/710-driverread-base and measure/710-driverread-fix branches, never in a
// PR. The fix moves the worker's dc.mu critical section in driverRead from after
// the read syscall to around it (Loop.Write already holds dc.mu across its write
// syscall, flushDriverSendLocked).
//
// v2 (unchanged): a bare Loop (no engine, no epoll), driverRead called directly
// over an AF_UNIX socketpair.
//   Uncontended512, Uncontended32K: each op writes one message into the peer and
//     drains it with driverRead.
//   Contended512: the same, while another goroutine calls Loop.Write on the same
//     conn in a tight loop, so the two sides compete for dc.mu. writes/s is that
//     writer's progress.
// v3 (new): a running epoll engine, one driver conn over TCP loopback, shaped like
// the redis driver's async path (a FIFO of waiters, a writer mutex around
// WorkerLoop.Write, replies completed in order by the worker's onRecv).
//   PipelinedG<g>: g callers share the conn; each enqueues its waiter and calls
//     Write under the writer mutex, then waits for its reply. The peer is a server
//     goroutine that answers every 32-byte request with a 128-byte reply, one
//     write per read. With g > 1, Writes stay in flight while replies stream into
//     driverRead. One op is one request and its reply.
//   PipelinedG16x4K: 16 callers, 4 KiB replies (the reads under dc.mu fill the
//     32 KiB buffer).
// TestBench710DriverRead runs every benchmark through testing.Benchmark and logs
// one BENCH710 line per benchmark, for celeris-stress timing mode, which runs
// tests, not -bench.

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

func benchDriverRead(b *testing.B, msg int, contended bool) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		b.Fatal(err)
	}
	local, peer := pair[0], pair[1]
	defer func() { _ = unix.Close(local); _ = unix.Close(peer) }()
	for _, fd := range pair {
		if err := unix.SetNonblock(fd, true); err != nil {
			b.Fatal(err)
		}
	}
	var got, writes atomic.Int64
	dc := &driverConn{fd: local, onRecv: func(p []byte) { got.Add(int64(len(p))) }, onClose: func(error) {}}
	// A bare Loop: driverRead touches only driverReadBuf (and closeDriver on EOF/error, which does not
	// happen here). ctlClosed refuses the EPOLL_CTL_MOD a Write's EAGAIN would issue.
	l := &Loop{driverConns: map[int]*driverConn{local: dc}, ctlClosed: true}
	l.hasDriverConns.Store(true)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	if contended {
		wg.Add(2)
		go func() { // the driver's writer
			defer wg.Done()
			w := make([]byte, 64)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := l.Write(local, w); err != nil {
					// ErrQueueFull cannot happen at 64 B/op with the drainer below; anything else is fatal.
					panic(err)
				}
				writes.Add(1)
			}
		}()
		go func() { // drains what the writer sends, so its writes keep reaching the kernel
			defer wg.Done()
			buf := make([]byte, 64<<10)
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = unix.Read(peer, buf)
			}
		}()
	}
	data := make([]byte, msg)
	b.SetBytes(int64(msg))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for off := 0; off < msg; {
			n, err := unix.Write(peer, data[off:])
			if err != nil {
				if err == unix.EAGAIN {
					continue
				}
				b.Fatal(err)
			}
			off += n
		}
		l.driverRead(dc)
	}
	b.StopTimer()
	close(stop)
	wg.Wait()
	if contended {
		// The other side of the contention: the writer's Loop.Write calls per second of the run.
		b.ReportMetric(float64(writes.Load())/b.Elapsed().Seconds(), "writes/s")
	}
	if got.Load() != int64(b.N)*int64(msg) {
		b.Fatalf("onRecv got %d bytes, want %d", got.Load(), int64(b.N)*int64(msg))
	}
}

// newBenchEngine710 is driver_test.go's newTestEngine for a testing.TB: a
// two-worker epoll engine on a free loopback port, with a no-op handler.
func newBenchEngine710(tb testing.TB) (*Engine, func()) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("probe listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg := resource.Config{Addr: addr, Resources: resource.Resources{Workers: 2}}
	eng, err := New(cfg, stream.HandlerFunc(func(_ context.Context, _ *stream.Stream) error { return nil }))
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = eng.Listen(ctx)
		close(done)
	}()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && (eng.Addr() == nil || eng.NumWorkers() == 0) {
		time.Sleep(2 * time.Millisecond)
	}
	if eng.Addr() == nil {
		cancel()
		<-done
		tb.Fatalf("engine did not bind within deadline")
	}
	return eng, func() { cancel(); <-done }
}

// dialDriverTCP710 returns a connected, non-blocking TCP_NODELAY client socket
// (the driver's descriptor) and the server's end of it.
func dialDriverTCP710(tb testing.TB) (int, net.Conn, net.Listener) {
	tb.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		tb.Fatal(err)
	}
	sa := &unix.SockaddrInet4{Port: ln.Addr().(*net.TCPAddr).Port, Addr: [4]byte{127, 0, 0, 1}}
	if err := unix.Connect(fd, sa); err != nil {
		tb.Fatal(err)
	}
	if err := unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1); err != nil {
		tb.Fatal(err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		tb.Fatal(err)
	}
	srv, err := ln.Accept()
	if err != nil {
		tb.Fatal(err)
	}
	return fd, srv, ln
}

func benchPipelined(b *testing.B, g, reqSize, repSize int) {
	eng, stop := newBenchEngine710(b)
	defer stop()
	wl := eng.WorkerLoop(0)
	fd, srv, ln := dialDriverTCP710(b)
	defer func() { _ = ln.Close() }()

	srvDone := make(chan struct{})
	go func() { // the server: one reply per complete request, all of a read's replies in one write
		defer close(srvDone)
		in := make([]byte, 64<<10)
		var out []byte
		partial := 0
		for {
			n, err := srv.Read(in)
			if err != nil {
				return
			}
			partial += n
			k := partial / reqSize
			partial -= k * reqSize
			if k == 0 {
				continue
			}
			if cap(out) < k*repSize {
				out = make([]byte, k*repSize)
			}
			if _, err := srv.Write(out[:k*repSize]); err != nil {
				return
			}
		}
	}()

	// The driver: waiters are queued in wire order (under wmu, before the Write), and the worker completes
	// them in that order, one per repSize bytes. A waiter is queued before its request is written, so the
	// worker never waits on q.
	q := make(chan chan struct{}, g)
	carry := 0
	onRecv := func(p []byte) {
		carry += len(p)
		for carry >= repSize {
			carry -= repSize
			(<-q) <- struct{}{}
		}
	}
	if err := wl.RegisterConn(fd, onRecv, func(error) {}); err != nil {
		b.Fatal(err)
	}
	var left atomic.Int64
	left.Store(int64(b.N))
	var wmu sync.Mutex
	req := make([]byte, reqSize)
	var wg sync.WaitGroup
	b.ReportAllocs()
	b.ResetTimer()
	for range g {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := make(chan struct{}, 1)
			for left.Add(-1) >= 0 {
				wmu.Lock()
				q <- done
				err := wl.Write(fd, req)
				wmu.Unlock()
				if err != nil {
					panic(err) // the rig is broken; a queued waiter would never complete
				}
				<-done
			}
		}()
	}
	wg.Wait()
	b.StopTimer()
	_ = wl.UnregisterConn(fd)
	_ = unix.Close(fd)
	_ = srv.Close()
	<-srvDone
}

func BenchmarkDriverRead710Uncontended512(b *testing.B) { benchDriverRead(b, 512, false) }
func BenchmarkDriverRead710Uncontended32K(b *testing.B) { benchDriverRead(b, 32<<10, false) }
func BenchmarkDriverRead710Contended512(b *testing.B)   { benchDriverRead(b, 512, true) }
func BenchmarkDriverRead710PipelinedG1(b *testing.B)    { benchPipelined(b, 1, 32, 128) }
func BenchmarkDriverRead710PipelinedG16(b *testing.B)   { benchPipelined(b, 16, 32, 128) }
func BenchmarkDriverRead710PipelinedG128(b *testing.B)  { benchPipelined(b, 128, 32, 128) }
func BenchmarkDriverRead710PipelinedG16x4K(b *testing.B) {
	benchPipelined(b, 16, 32, 4<<10)
}

// TestBench710DriverRead is the timing-mode wrapper: every benchmark above,
// through testing.Benchmark (the default 1 s benchtime), one log line each:
//
//	BENCH710 shape=<name> n=<N> ns_per_op=<x> writes_per_s=<y or -> allocs_per_op=<a> bytes_per_op=<b>
func TestBench710DriverRead(t *testing.T) {
	for _, bm := range []struct {
		name string
		fn   func(*testing.B)
	}{
		{"Uncontended512", BenchmarkDriverRead710Uncontended512},
		{"Uncontended32K", BenchmarkDriverRead710Uncontended32K},
		{"Contended512", BenchmarkDriverRead710Contended512},
		{"PipelinedG1", BenchmarkDriverRead710PipelinedG1},
		{"PipelinedG16", BenchmarkDriverRead710PipelinedG16},
		{"PipelinedG128", BenchmarkDriverRead710PipelinedG128},
		{"PipelinedG16x4K", BenchmarkDriverRead710PipelinedG16x4K},
	} {
		r := testing.Benchmark(bm.fn)
		if r.N == 0 {
			t.Errorf("BENCH710 shape=%s: the benchmark failed", bm.name)
			continue
		}
		w := "-"
		if v, ok := r.Extra["writes/s"]; ok {
			w = strconv.FormatFloat(v, 'f', 0, 64)
		}
		t.Logf("BENCH710 shape=%s n=%d ns_per_op=%.1f writes_per_s=%s allocs_per_op=%d bytes_per_op=%d",
			bm.name, r.N, float64(r.T.Nanoseconds())/float64(r.N), w, r.AllocsPerOp(), r.AllocedBytesPerOp())
	}
}
