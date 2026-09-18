//go:build linux

// Package deferab measures what TCP_DEFER_ACCEPT on the epoll and io_uring
// listen sockets is worth, and checks that celeris#662's fix leaves the steady
// state alone.
//
// With the option off, a connection whose first request has not arrived by
// accept time costs one extra wakeup (an epoll readiness after EPOLL_CTL_ADD
// with no data, or an io_uring recv armed with no data). That is about nil for
// keep-alive traffic and potentially material for connection churn. The fix
// for celeris#662 keeps the option on every listener in the steady state and
// clears it only on a listener that is pausing, so a run of these benchmarks,
// which never pauses, must look like one on main; a variant with the option
// off prices what keeping it is worth.
//
// Four workloads, each on both engines:
//
//	ChurnImmediate  connect, write the request at once, read, close.
//	                The data usually arrives right behind the handshake, so
//	                the extra wakeup happens only when it does not: this is
//	                the realistic churn case.
//	ChurnDelayed    connect, wait churnDelay, then write. The delay is three
//	                orders of magnitude above the accept path's latency and
//	                three below the kernel's ~1s SYN-ACK retransmit, so the
//	                extra wakeup happens on essentially every connection.
//	                This is the worst realistic case.
//	ChurnSilent     connect and close, never writing. With the option ON the
//	                engine never sees the connection at all; with it OFF the
//	                engine accepts, arms, and tears down. This is the
//	                absolute worst case -- and it is also exactly the
//	                connection class celeris#662 says the engine MUST see.
//	KeepAlive       a fixed set of connections issuing many requests each.
//	                One accept is amortised over b.N requests, so this arm
//	                should show nothing, and its job is to say so.
//
// Every benchmark reports, beside ns/op:
//
//	cpu_us/op    process CPU (utime+stime) over the timed region per op. The
//	             client half of the process does identical work in both arms,
//	             so a CPU delta is the server's. It is the mechanism-level
//	             outcome and it is far more sensitive than wall time.
//	accepts/op   engine AcceptCount delta per op -- the behavioural witness
//	             that the arm swap took effect (Silent: ~0 with the option
//	             on, ~1 with it off).
//	deferdrop/op netns TcpExtTCPDeferAcceptDrop delta per op -- the kernel's
//	             own witness that it is holding handshake-complete
//	             connections out of the accept queue. ~1 with the option on,
//	             0 with it off.
//	reqs/op      engine RequestCount delta per op -- correctness.
//	srverr/op    engine ErrorCount delta per op.
//	workers      the engine's worker count, which RLIMIT_MEMLOCK caps for
//	             io_uring. No cross-arm reading is safe without it.
//	listeners    LISTEN sockets of this process on the engine's port, read
//	             through /proc/self/fd before the timed region.
//	listeners_defer_off
//	             how many of those read TCP_DEFER_ACCEPT off: the socket
//	             witness. 0 means the steady state has the option on every
//	             listener; equal to listeners means it has none.
//
// Client sockets are closed with SO_LINGER 0 so churn produces no TIME_WAIT
// and the run cannot drift into ephemeral-port exhaustion partway through.
// The teardown path is identical in both arms, so it cannot carry the effect.
package deferab

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/adaptive"
	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/engine/epoll"
	"github.com/goceleris/celeris/engine/iouring"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

const (
	// req is the smallest well-formed H1 request.
	req = "GET / HTTP/1.1\r\nHost: x\r\n\r\n"
	// engineWorkers is requested from both engines. io_uring may be capped
	// below it by RLIMIT_MEMLOCK; the achieved count is reported as
	// "workers" on every line.
	engineWorkers = 4
	// churnConc is the client concurrency for the two churn arms that do
	// not sleep. Base-only calibration (amendment A1) measured 34.53
	// cpu_us/op against 23.888 ns/op elapsed at conc=32 -- 1.45 of 4 CPUs
	// busy, i.e. latency-bound, where ns/op cannot register server-side
	// work at all. 128 saturates the container.
	churnConc = 128
	// delayedConc is higher still because each op parks for churnDelay: at
	// 256 the sleep ceiling (256/500us = 512k ops/s) sits far above the CPU
	// ceiling (4 / 34us = 116k ops/s), so the delayed arm is CPU-bound too
	// rather than measuring its own sleep.
	delayedConc = 256
	// churnDelay separates the handshake from the first byte. Far above the
	// accept path (tens of microseconds), far below the kernel's ~1s
	// SYN-ACK retransmit.
	churnDelay = 500 * time.Microsecond
	// kaConns is the number of persistent connections in the keep-alive arm.
	kaConns = 32
)

// okHandler answers every request with a fixed two-byte body. Nothing here
// blocks: the cost under test is on the accept path, so the handler must not
// be where the time goes.
type okHandler struct{}

func (okHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}

// liveEngine is the slice of *epoll.Engine and *iouring.Engine this file uses.
type liveEngine interface {
	Listen(context.Context) error
	Shutdown(context.Context) error
	Addr() net.Addr
	Metrics() engine.EngineMetrics
	NumWorkers() int
}

// nextPort hands out listen ports from BELOW the ephemeral range
// (/proc/sys/net/ipv4/ip_local_port_range starts at 32768 in the container).
// Picking a port with net.Listen(":0") and handing the freed number to the
// engine is the standard trick and it races with the run's own client sockets
// once the run makes tens of thousands of them; a fixed low port cannot
// collide with an ephemeral allocation at all.
var nextPort atomic.Int32

func init() { nextPort.Store(21000) }

func reservePort(b *testing.B) string {
	b.Helper()
	for range 500 {
		addr := "127.0.0.1:" + strconv.Itoa(int(nextPort.Add(1)))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}
		_ = ln.Close()
		return addr
	}
	b.Fatal("no free port in the reserved range")
	return ""
}

// deferAcceptDrops reads the netns-wide TcpExtTCPDeferAcceptDrop counter: the
// kernel bumps it every time it drops a bare handshake ACK because
// TCP_DEFER_ACCEPT is set on the listener. Reported, never asserted -- the
// counter is per network namespace, and the container's namespace is quiet but
// not provably empty.
func deferAcceptDrops() uint64 {
	b, err := os.ReadFile("/proc/net/netstat")
	if err != nil {
		return 0
	}
	lines := strings.Split(string(b), "\n")
	for i := 0; i+1 < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "TcpExt:") || !strings.HasPrefix(lines[i+1], "TcpExt:") {
			continue
		}
		names := strings.Fields(lines[i])
		vals := strings.Fields(lines[i+1])
		if len(names) != len(vals) {
			continue
		}
		for j, n := range names {
			if n == "TCPDeferAcceptDrop" {
				v, perr := strconv.ParseUint(vals[j], 10, 64)
				if perr != nil {
					return 0
				}
				return v
			}
		}
	}
	return 0
}

func startEngine(b *testing.B, kind string) (liveEngine, string, func()) {
	b.Helper()
	cfg := resource.Config{
		Addr:      reservePort(b),
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: engineWorkers},
		Logger:    slog.New(slog.DiscardHandler),
	}

	var (
		e   liveEngine
		err error
	)
	switch kind {
	case "epoll":
		e, err = epoll.New(cfg, okHandler{})
	case "iouring":
		e, err = iouring.New(cfg, okHandler{})
	case "adaptive":
		// The DEFAULT engine on Linux (resource.defaultEngine). Adaptive
		// builds its sub-engines from this cfg, which leaves
		// DisableDeferAccept to the caller, so its listeners have the option
		// exactly when the standalone engines' do.
		//
		// Switching is FROZEN for the whole run. The cost under test is on
		// the accept path; a promotion landing mid-benchmark would measure
		// the switch instead, and at 128 concurrent churned connections over
		// 4 workers the controller's up-threshold (24 conns/worker) would be
		// crossed. The freeze does not touch how the listen sockets were
		// created, which is the only thing the arms differ in.
		var ae *adaptive.Engine
		ae, err = adaptive.New(cfg, okHandler{}, nil)
		if err == nil {
			ae.FreezeSwitching()
			e = ae
		}
	default:
		b.Fatalf("unknown engine %q", kind)
	}
	if err != nil {
		b.Fatalf("New(%s): %v", kind, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()

	for dl := time.Now().Add(20 * time.Second); time.Now().Before(dl); {
		if e.Addr() != nil && e.NumWorkers() > 0 {
			break
		}
		select {
		case lerr := <-done:
			cancel()
			b.Fatalf("%s Listen returned before binding: %v", kind, lerr)
		default:
		}
		time.Sleep(2 * time.Millisecond)
	}
	if e.Addr() == nil || e.NumWorkers() == 0 {
		cancel()
		b.Fatalf("%s never bound with workers", kind)
	}

	stop := func() {
		_ = e.Shutdown(context.Background())
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			b.Error("engine did not stop within 20s")
		}
	}
	return e, e.Addr().String(), stop
}

// probeResponse does one request on its own connection, before the timed
// region, and returns the exact response length. Every later response is read
// with io.ReadFull for that length and checked for the "ok" body, so a
// response that changed size desynchronises loudly instead of silently
// shortening the measured work.
func probeResponse(b *testing.B, addr string) int {
	b.Helper()
	c, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		b.Fatalf("probe dial: %v", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	if _, werr := c.Write([]byte(req)); werr != nil {
		b.Fatalf("probe write: %v", werr)
	}
	buf := make([]byte, 8192)
	n := 0
	for {
		m, rerr := c.Read(buf[n:])
		if rerr != nil {
			b.Fatalf("probe read after %d bytes (%q): %v", n, buf[:n], rerr)
		}
		n += m
		if i := strings.Index(string(buf[:n]), "\r\n\r\n"); i >= 0 && n >= i+6 {
			break
		}
		if n == len(buf) {
			b.Fatalf("probe response exceeded %d bytes", len(buf))
		}
	}
	if !strings.HasSuffix(string(buf[:n]), "ok") {
		b.Fatalf("probe response %q does not end in the expected body", buf[:n])
	}
	return n
}

// counters is the set of non-timing outcomes every arm reports.
type counters struct {
	base   engine.EngineMetrics
	ru     unix.Rusage
	drops  uint64
	cliErr atomic.Int64
	// The socket witness, read before the timed region.
	listeners, deferOff int
}

// listenWitness counts this process's LISTEN sockets on port and how many of
// them read TCP_DEFER_ACCEPT off. It reads /proc/self/fd, so another process
// holding the port cannot confuse it, and a getsockopt on a descriptor number
// touches no memory the engine owns.
func listenWitness(port int) (listeners, deferOff int) {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1, -1
	}
	for _, ent := range ents {
		fd, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		link, err := os.Readlink("/proc/self/fd/" + ent.Name())
		if err != nil || !strings.HasPrefix(link, "socket:[") {
			continue
		}
		if v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN); err != nil || v != 1 {
			continue
		}
		sa, err := unix.Getsockname(fd)
		if err != nil {
			continue
		}
		p := -1
		switch v := sa.(type) {
		case *unix.SockaddrInet4:
			p = v.Port
		case *unix.SockaddrInet6:
			p = v.Port
		}
		if p != port {
			continue
		}
		listeners++
		if d, err := unix.GetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT); err == nil && d == 0 {
			deferOff++
		}
	}
	return listeners, deferOff
}

func (c *counters) start(b *testing.B, e liveEngine) {
	b.Helper()
	if ta, ok := e.Addr().(*net.TCPAddr); ok {
		c.listeners, c.deferOff = listenWitness(ta.Port)
	}
	runtime.GC()
	c.base = e.Metrics()
	c.drops = deferAcceptDrops()
	if err := unix.Getrusage(unix.RUSAGE_SELF, &c.ru); err != nil {
		b.Fatalf("getrusage: %v", err)
	}
}

func (c *counters) finish(b *testing.B, e liveEngine, nw int) {
	b.Helper()
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		b.Fatalf("getrusage: %v", err)
	}
	cpuNs := (ru.Utime.Nano() + ru.Stime.Nano()) - (c.ru.Utime.Nano() + c.ru.Stime.Nano())
	m := e.Metrics()
	n := float64(b.N)

	b.ReportMetric(float64(cpuNs)/1000.0/n, "cpu_us/op")
	b.ReportMetric(float64(m.AcceptCount-c.base.AcceptCount)/n, "accepts/op")
	b.ReportMetric(float64(deferAcceptDrops()-c.drops)/n, "deferdrop/op")
	b.ReportMetric(float64(m.RequestCount-c.base.RequestCount)/n, "reqs/op")
	b.ReportMetric(float64(m.ErrorCount-c.base.ErrorCount)/n, "srverr/op")
	b.ReportMetric(float64(nw), "workers")
	b.ReportMetric(float64(c.listeners), "listeners")
	b.ReportMetric(float64(c.deferOff), "listeners_defer_off")

	// A client-side failure means the rig did not measure what it claims to
	// measure, so it fails the round rather than quietly shrinking the work.
	// failrate.py counts failed rounds with their own denominator.
	if got := c.cliErr.Load(); got != 0 {
		b.Fatalf("%d client-side failures in %d ops: this round measured a broken rig, "+
			"not the option under test", got, b.N)
	}
}

func benchChurn(b *testing.B, kind string, delay time.Duration, silent bool, conc int) {
	e, addr, stop := startEngine(b, kind)
	defer stop()
	nw := e.NumWorkers()
	respLen := probeResponse(b, addr)

	var c counters
	var remaining atomic.Int64
	remaining.Store(int64(b.N))

	c.start(b, e)
	b.ResetTimer()

	var wg sync.WaitGroup
	for range conc {
		wg.Go(func() {
			buf := make([]byte, respLen)
			for remaining.Add(-1) >= 0 {
				conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
				if err != nil {
					c.cliErr.Add(1)
					continue
				}
				if tc, ok := conn.(*net.TCPConn); ok {
					// RST on close: no TIME_WAIT, so a long churn run
					// cannot exhaust the ephemeral range partway through
					// and change what the later rounds measure.
					_ = tc.SetLinger(0)
				}
				if silent {
					_ = conn.Close()
					continue
				}
				if delay > 0 {
					time.Sleep(delay)
				}
				_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
				if _, werr := conn.Write([]byte(req)); werr != nil {
					c.cliErr.Add(1)
					_ = conn.Close()
					continue
				}
				if _, rerr := io.ReadFull(conn, buf); rerr != nil {
					c.cliErr.Add(1)
					_ = conn.Close()
					continue
				}
				if string(buf[respLen-2:]) != "ok" {
					c.cliErr.Add(1)
				}
				_ = conn.Close()
			}
		})
	}
	wg.Wait()

	b.StopTimer()
	c.finish(b, e, nw)
}

func benchKeepAlive(b *testing.B, kind string, conns int) {
	e, addr, stop := startEngine(b, kind)
	defer stop()
	nw := e.NumWorkers()
	respLen := probeResponse(b, addr)

	cs := make([]net.Conn, 0, conns)
	defer func() {
		for _, conn := range cs {
			_ = conn.Close()
		}
	}()
	warm := make([]byte, respLen)
	for range conns {
		conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
		if err != nil {
			b.Fatalf("keep-alive dial: %v", err)
		}
		cs = append(cs, conn)
		// Establish and serve each connection BEFORE the timed region, so
		// the accepts are not inside the measurement at all: that is the
		// whole point of this arm.
		_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
		if _, werr := conn.Write([]byte(req)); werr != nil {
			b.Fatalf("keep-alive warm write: %v", werr)
		}
		if _, rerr := io.ReadFull(conn, warm); rerr != nil {
			b.Fatalf("keep-alive warm read: %v", rerr)
		}
	}

	var c counters
	var remaining atomic.Int64
	remaining.Store(int64(b.N))

	c.start(b, e)
	b.ResetTimer()

	var wg sync.WaitGroup
	for _, conn := range cs {
		wg.Go(func() {
			buf := make([]byte, respLen)
			for remaining.Add(-1) >= 0 {
				_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
				if _, werr := conn.Write([]byte(req)); werr != nil {
					c.cliErr.Add(1)
					return
				}
				if _, rerr := io.ReadFull(conn, buf); rerr != nil {
					c.cliErr.Add(1)
					return
				}
				if string(buf[respLen-2:]) != "ok" {
					c.cliErr.Add(1)
					return
				}
			}
		})
	}
	wg.Wait()

	b.StopTimer()
	c.finish(b, e, nw)
}

func BenchmarkChurnImmediateEpoll(b *testing.B)   { benchChurn(b, "epoll", 0, false, churnConc) }
func BenchmarkChurnImmediateIouring(b *testing.B) { benchChurn(b, "iouring", 0, false, churnConc) }

func BenchmarkChurnDelayedEpoll(b *testing.B) {
	benchChurn(b, "epoll", churnDelay, false, delayedConc)
}

func BenchmarkChurnDelayedIouring(b *testing.B) {
	benchChurn(b, "iouring", churnDelay, false, delayedConc)
}

func BenchmarkChurnSilentEpoll(b *testing.B)   { benchChurn(b, "epoll", 0, true, churnConc) }
func BenchmarkChurnSilentIouring(b *testing.B) { benchChurn(b, "iouring", 0, true, churnConc) }

func BenchmarkKeepAliveEpoll(b *testing.B)   { benchKeepAlive(b, "epoll", kaConns) }
func BenchmarkKeepAliveIouring(b *testing.B) { benchKeepAlive(b, "iouring", kaConns) }

// The ADAPTIVE arms (celeris#662 B1). Adaptive is the default engine on
// Linux, so these are the cells that decide whether the fix lands a
// regression on the default configuration.
//
// listeners_defer_off and deferdrop/op are the per-round witnesses of the
// option, read before any timing: listeners_defer_off 0 and deferdrop/op ~1
// on the churn arms mean every listener kept TCP_DEFER_ACCEPT.

func BenchmarkChurnImmediateAdaptive(b *testing.B) {
	benchChurn(b, "adaptive", 0, false, churnConc)
}

func BenchmarkChurnDelayedAdaptive(b *testing.B) {
	benchChurn(b, "adaptive", churnDelay, false, delayedConc)
}

func BenchmarkChurnSilentAdaptive(b *testing.B) { benchChurn(b, "adaptive", 0, true, churnConc) }

func BenchmarkKeepAliveAdaptive(b *testing.B) { benchKeepAlive(b, "adaptive", kaConns) }
