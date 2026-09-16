//go:build linux

package websocket

import (
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris"
	celerisengine "github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
)

// This file is the A/B apparatus for celeris#667. The fix moves the engine's
// pause()/resume() callbacks INSIDE the pausedMu critical section that
// decides them, which puts the engine's detachQMu — and, on the detach
// queue's empty->non-empty edge, an eventfd write — inside a lock the handler
// goroutine takes on every chunk it reads. On io_uring the appending side is
// the single worker thread, so any added serialization lands there.
//
// The benchmarks below exist to measure that cost rather than argue about it.
// Both arms run this identical file; only middleware/websocket/engineread.go
// differs between them.

// enginePauseStub reproduces exactly what the engines' PauseRecv/ResumeRecv
// closures do (engine/iouring/worker.go:2200-2231 and
// engine/epoll/loop.go:1672-1703): an atomic Swap that early-returns on a
// no-op, a detach-queue append under detachQMu, and — only on the queue's
// empty->non-empty edge — a write to a non-blocking eventfd.
//
// Using a real eventfd matters: the write is the most expensive thing the fix
// pulls under pausedMu, and stubbing it out would understate the cost.
type enginePauseStub struct {
	desired atomic.Bool  // mirrors connState.recvPauseDesired
	pending atomic.Int32 // mirrors detachQPending
	qmu     sync.Mutex   // mirrors detachQMu
	queue   []int        // mirrors detachQueue
	efd     int          // mirrors the worker wakeup eventfd (EFD_NONBLOCK)

	pauseCalls  atomic.Int64 // callback invocations
	resumeCalls atomic.Int64
	edges       atomic.Int64 // Swap transitions that actually changed the engine
	wakeups     atomic.Int64 // eventfd writes performed
}

func newEnginePauseStub(tb testing.TB) *enginePauseStub {
	tb.Helper()
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		tb.Fatalf("eventfd: %v", err)
	}
	tb.Cleanup(func() { _ = unix.Close(efd) })
	return &enginePauseStub{efd: efd}
}

func (s *enginePauseStub) pause() {
	s.pauseCalls.Add(1)
	if s.desired.Swap(true) {
		return
	}
	s.edges.Add(1)
	s.enqueue()
}

func (s *enginePauseStub) resume() {
	s.resumeCalls.Add(1)
	if !s.desired.Swap(false) {
		return
	}
	s.edges.Add(1)
	s.enqueue()
}

func (s *enginePauseStub) enqueue() {
	s.qmu.Lock()
	s.queue = append(s.queue, 1)
	wasEmpty := s.pending.Swap(1) == 0
	s.qmu.Unlock()
	if wasEmpty && s.efd >= 0 {
		var val [8]byte
		val[0] = 1
		if _, err := unix.Write(s.efd, val[:]); err == nil {
			s.wakeups.Add(1)
		}
	}
}

// drain models drainDetachQueue on the worker thread: the slice swap under
// detachQMu plus the eventfd read that re-arms the wakeup edge, so subsequent
// edges pay for the write again instead of coalescing away.
func (s *enginePauseStub) drain() {
	if s.pending.Load() == 0 {
		return
	}
	s.qmu.Lock()
	s.queue = s.queue[:0]
	s.pending.Store(0)
	s.qmu.Unlock()
	var buf [8]byte
	_, _ = unix.Read(s.efd, buf[:])
}

// BenchmarkChanReaderContended is the primary A/B measurement: the two
// goroutines that contend for pausedMu in production, driven so the
// watermarks are crossed continuously.
//
//   - the producer is the engine worker thread. It models the worker in the
//     two ways that matter here: it delivers NOTHING while the engine's recv
//     is paused (a paused connection receives nothing), and it drains the
//     detach queue as it goes (the worker does that on every loop turn).
//   - the consumer is the handler goroutine, which takes pausedMu on every
//     single chunk it reads — the per-chunk lock the fix widens.
//
// cap16 is the pathological shape (a crossing every few chunks); cap256 is
// the production default (Config.MaxBackpressureBuffer).
func BenchmarkChanReaderContended(b *testing.B) {
	for _, capacity := range []int{16, 256} {
		b.Run("cap"+itoa(capacity), func(b *testing.B) {
			benchChanReaderContended(b, capacity)
		})
	}
}

func benchChanReaderContended(b *testing.B, capacity int) {
	stub := newEnginePauseStub(b)
	r := newChanReader(capacity, 0, 0)
	r.SetPauser(stub.pause, stub.resume)

	chunk := []byte{'x'}
	n := b.N

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1)
		for i := 0; i < n; i++ {
			if _, err := r.Read(buf); err != nil {
				b.Errorf("read %d: %v", i, err)
				return
			}
		}
	}()

	var forced int64

	b.ResetTimer()
	for i := 0; i < n; i++ {
		// A paused connection delivers nothing until the engine resumes it.
		for stub.desired.Load() {
			stub.drain()
			// Escape hatch for a PRE-EXISTING chanReader hole that is not
			// celeris#667 and is present in BOTH arms.
			//
			// requestPause decides on a depth snapshot. If the handler drains
			// the channel to EMPTY before that pause is applied, the reader is
			// left paused with nothing buffered — and Read only re-evaluates
			// the resume after it has successfully dequeued a chunk, which can
			// never happen again because the engine is paused. Producer and
			// consumer then wait on each other forever. Without this escape
			// the benchmark wedges (observed: consumer parked in Read's
			// blocking select, producer spinning on desired==true).
			//
			// Delivering anyway models an engine that still had chunks in
			// flight, unwedges the reader, and is counted so the two arms can
			// be checked for the same behaviour.
			if len(r.ch) == 0 {
				forced++
				break
			}
		}
		if !r.Append(chunk) {
			b.Fatalf("append %d rejected (ErrReadLimit) — the workload outran the spill", i)
		}
		stub.drain()
	}
	wg.Wait()
	b.StopTimer()

	// Reported so the two arms can be checked for the SAME trigger: if the
	// fix changed how often the watermarks are crossed, the ns/op comparison
	// would be meaningless.
	ops := float64(max(n, 1))
	b.ReportMetric(float64(stub.edges.Load())/ops, "edges/op")
	b.ReportMetric(float64(stub.pauseCalls.Load())/ops, "pausecb/op")
	b.ReportMetric(float64(stub.resumeCalls.Load())/ops, "resumecb/op")
	b.ReportMetric(float64(stub.wakeups.Load())/ops, "wakeups/op")
	b.ReportMetric(float64(forced)/ops, "forced/op")
}

// BenchmarkChanReaderNoEdges is the internal control. The watermarks are
// never crossed, so neither callback ever runs and the fix cannot change
// anything on this path — yet Read still takes pausedMu on every chunk. Any
// difference the A/B reports HERE is the harness's own noise, not a cost of
// the fix, which is what makes it the yardstick for reading the contended
// numbers.
//
// The zero-callback precondition is asserted, so the control cannot quietly
// stop being a control.
func BenchmarkChanReaderNoEdges(b *testing.B) {
	stub := newEnginePauseStub(b)
	r := newChanReader(256, 0, 0)
	r.SetPauser(stub.pause, stub.resume)

	chunk := []byte{'x'}
	buf := make([]byte, 1)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !r.Append(chunk) {
			b.Fatalf("append %d rejected", i)
		}
		if _, err := r.Read(buf); err != nil {
			b.Fatalf("read %d: %v", i, err)
		}
	}
	b.StopTimer()

	if got := stub.pauseCalls.Load() + stub.resumeCalls.Load(); got != 0 {
		b.Fatalf("control invalidated: %d pause/resume callbacks fired, so the "+
			"watermarks WERE crossed and this is no longer a no-edge path", got)
	}
}

// BenchmarkWSEngineBackpressureEcho is the ecological arm: a real native
// engine, a real WebSocket upgrade and real inbound backpressure, measuring
// end-to-end echo throughput.
//
// LIMITATION, stated up front: the chanReader watermark-crossing rate is not
// observable from outside the middleware, so this benchmark cannot prove how
// many pause/resume edges it generated. MaxBackpressureBuffer is set small
// and every frame is written with its own syscall (TCP_NODELAY) so that each
// frame tends to arrive as its own recv chunk — the watermarks count CHUNKS,
// not frames — but that is a design argument, not a measurement. The question
// this arm answers is "does the fix move end-to-end throughput"; the question
// "what does an edge cost" is answered by BenchmarkChanReaderContended, which
// counts its edges.
func BenchmarkWSEngineBackpressureEcho(b *testing.B) {
	for _, kind := range benchEngineKinds(b) {
		b.Run(kind.String(), func(b *testing.B) {
			benchWSEcho(b, kind)
		})
	}
}

func benchEngineKinds(tb testing.TB) []celeris.EngineType {
	tb.Helper()
	kinds := []celeris.EngineType{celeris.Epoll}
	p := probe.Probe()
	if p.IOUringTier >= celerisengine.High && p.ProvidedBuffers {
		kinds = append(kinds, celeris.IOUring)
	} else {
		tb.Logf("io_uring tier=%s kernel=%s — excluded from the benchmark matrix",
			p.IOUringTier.String(), p.KernelVersion)
	}
	return kinds
}

func benchWSEcho(b *testing.B, kind celeris.EngineType) {
	clients := envInt("WS667_CLIENTS", 8)
	framesPerOp := envInt("WS667_FRAMES", 64)
	bpBuf := envInt("WS667_BP", 16)
	const plen = 120

	addr, shutdown, srv := startNativeServerWithHandle(b, kind, Config{
		CheckOrigin:           func(*celeris.Context) bool { return true },
		ReadLimit:             1 << 20,
		MaxBackpressureBuffer: bpBuf,
		Handler: func(c *Conn) {
			for {
				mt, msg, err := c.ReadMessage()
				if err != nil {
					return
				}
				if err := c.WriteMessage(mt, msg); err != nil {
					return
				}
			}
		},
	})
	defer shutdown()

	// The worker count is the memlock-sensitive variable this A/B is run
	// across (8 MiB gives io_uring ONE worker, 128 MiB gives it four), so it
	// is recorded in the log rather than assumed.
	if info := srv.EngineInfo(); info != nil {
		b.Logf("%s: workers=%d bpBuf=%d clients=%d framesPerOp=%d",
			kind, info.Metrics.Workers, bpBuf, clients, framesPerOp)
	} else {
		b.Logf("%s: workers=UNKNOWN (EngineInfo nil) bpBuf=%d", kind, bpBuf)
	}

	hostPort := strings.TrimPrefix(addr, "ws://")
	hostPort = strings.TrimSuffix(hostPort, "/ws")

	frame := maskedTextFrames(1, plen)
	echoLen := 2 + plen

	dialer := net.Dialer{Timeout: 5 * time.Second, Control: func(_, _ string, rc syscall.RawConn) error {
		var serr error
		_ = rc.Control(func(fd uintptr) {
			// One segment per frame, so each frame tends to become its own
			// recv chunk and the chunk-counting watermarks actually move.
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
		})
		return serr
	}}

	conns := make([]net.Conn, 0, clients)
	bufs := make([][]byte, clients)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < clients; i++ {
		c, err := dialer.Dial("tcp", hostPort)
		if err != nil {
			b.Fatalf("dial %d: %v", i, err)
		}
		conns = append(conns, c)
		if err := wsHandshake(c, hostPort); err != nil {
			b.Fatalf("handshake %d: %v", i, err)
		}
		bufs[i] = make([]byte, 32<<10)
	}

	errs := make([]error, clients)

	b.SetBytes(int64(clients * framesPerOp * echoLen))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		for ci := range conns {
			wg.Add(1)
			go func(ci int) {
				defer wg.Done()
				c := conns[ci]
				// Writing then reading in one goroutine is safe at this size:
				// framesPerOp*echoLen stays well inside the socket buffers, so
				// the server's echo never blocks this client's writes.
				_ = c.SetWriteDeadline(time.Now().Add(60 * time.Second))
				for f := 0; f < framesPerOp; f++ {
					if _, err := c.Write(frame); err != nil {
						errs[ci] = err
						return
					}
				}
				_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
				buf := bufs[ci]
				need := framesPerOp * echoLen
				for need > 0 {
					n, err := c.Read(buf[:min(len(buf), need)])
					if err != nil {
						errs[ci] = err
						return
					}
					need -= n
				}
			}(ci)
		}
		wg.Wait()
		for ci, err := range errs {
			if err != nil {
				b.Fatalf("client %d: %v", ci, err)
			}
		}
	}
	b.StopTimer()
}

// itoa avoids pulling strconv in just for the sub-benchmark names.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d [20]byte
	i := len(d)
	for n > 0 {
		i--
		d[i] = byte('0' + n%10)
		n /= 10
	}
	return string(d[i:])
}
