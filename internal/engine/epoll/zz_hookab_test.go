//go:build linux

package epoll

// HOOKAB (RULE 74 for celeris#1086 and #1087), evidence only. This file lives on
// the measure/hookab-* branches, never in a PR, and is byte-identical on every
// arm. It compiles against main alone: it names no hook and no symbol a PR adds.
//
// Why a test and not a benchmark: celeris-stress timing mode (cluster) runs
// tests, not -bench, and pins every observation to ONE CPU on msr1 (its fastest
// core class has one). So every shape here parks or blocks and none spins:
// BenchmarkDriverPingPong930 and BenchmarkDriverFloodThroughput930 spin on an
// atomic (c930FeederSpin) and would starve the loop on one CPU.
//
// The shapes (one HOOKAB line each per iteration of the test, see below):
//
//   - DriverReadBare1B, DriverReadBareFlood: a bare Loop, driverRead called
//     directly over an AF_UNIX socketpair, as zz_bench710_driverread_test.go
//     did. 1B: one op is a one-byte write and the driverRead that drains it
//     (the read the #1086 seam sits in, once per op). Flood: one op fills the
//     socket to EAGAIN and one driverRead drains it with up to
//     driverReadBudget (8) reads of driverReadBufSize each, so the seam runs on
//     every one of up to eight reads per turn. ns_per_op is per onRecv
//     callback (per read(2)).
//   - DriverCloseBare: a bare Loop with a real epoll descriptor; one op registers
//     a fresh socketpair end, closes the peer and lets driverRead see the EOF,
//     which takes closeDriver (the #1086 testHookCloseDriverDeleted seam, once
//     per op).
//   - PingPong1B: a running engine, one driver conn registered with
//     WorkerLoop(0) over a socketpair: one op is a one-byte write by the peer,
//     the worker's epoll wake, its driverRead and the onRecv, which wakes the
//     blocked caller through a channel. Per-op latency gives p99.
//   - HijackSync, HijackAsyncInline: a Workers:2 engine, one op is a new TCP
//     connection, GET /hj, the handler hijacks, writes a response on the raw
//     conn and closes it, the client reads to EOF. Sync: AsyncHandlers off.
//     AsyncInline: AsyncHandlers on and no async route, so /hj runs inline in
//     InlineMode. Both take hijackConn's inline branch (the #1087 seam, once
//     per op). Per-op latency gives p99.
//
// TestHookAB logs, per shape and per iteration of the test (go test -count):
//
//	HOOKAB shape=<name> n=<ops> ns_per_op=<x> cpu_ns_per_op=<y> p99_ns=<z or -> errors=<e> allocs_per_op=<a> reads=<r> promoted=<p>
//
// cpu_ns_per_op is the process's user+system CPU time (getrusage) over the
// shape's timed section per op; allocs_per_op the process's mallocs over the
// same section per op (exact only for the bare shapes). promoted is the number of async promotions during a hijack shape's timed
// section (AsyncHandlers on promotes an unmarked handler slower than about 300 us; a promoted conn hijacks off the loop thread,
// through the branch that has no hook).

import (
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

const (
	hookabBareN     = 1000000 // DriverReadBare1B ops
	hookabFloodN    = 4000    // DriverReadBareFlood ops (fill + drain)
	hookabPingN     = 60000   // PingPong1B ops
	hookabHijackN   = 4000    // Hijack* ops (one connection each)
	hookabCloseN    = 40000   // DriverCloseBare ops (one socketpair each)
	hookabOpTimeout = 5 * time.Second
)

// hookabStats is what one shape's timed section measured.
type hookabStats struct {
	shape   string
	n       int   // operations
	reads   int64 // onRecv callbacks (bare shapes), else 0
	wall    time.Duration
	cpu     time.Duration
	mallocs uint64
	lat     []time.Duration // per-op latencies, nil when not taken
	errors  int
	// promoted: async promotions (Metrics().AsyncPromotedConns) during the timed section of a hijack shape; 0 for the others.
	promoted int64
}

func hookabCPU() time.Duration {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func hookabMallocs() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.Mallocs
}

// begin/end bracket the timed section.
func (s *hookabStats) begin() (t0 time.Time, c0 time.Duration, m0 uint64) {
	runtime.GC()
	return time.Now(), hookabCPU(), hookabMallocs()
}

func (s *hookabStats) end(t0 time.Time, c0 time.Duration, m0 uint64) {
	s.wall = time.Since(t0)
	s.cpu = hookabCPU() - c0
	s.mallocs = hookabMallocs() - m0
}

func (s *hookabStats) line() string {
	per := s.n
	if s.reads > 0 {
		per = int(s.reads)
	}
	if per == 0 {
		per = 1
	}
	p99 := "-"
	if len(s.lat) > 0 {
		l := append([]time.Duration(nil), s.lat...)
		sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
		p99 = fmt.Sprintf("%d", l[int(0.99*float64(len(l)-1))].Nanoseconds())
	}
	return fmt.Sprintf("HOOKAB shape=%s n=%d ns_per_op=%.2f cpu_ns_per_op=%.2f p99_ns=%s errors=%d allocs_per_op=%.3f reads=%d promoted=%d",
		s.shape, s.n, float64(s.wall.Nanoseconds())/float64(per), float64(s.cpu.Nanoseconds())/float64(per),
		p99, s.errors, float64(s.mallocs)/float64(per), s.reads, s.promoted)
}

// hookabPair returns a connected AF_UNIX stream socketpair, both ends non-blocking.
func hookabPair(tb testing.TB) (local, peer int) {
	tb.Helper()
	p, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		tb.Fatalf("socketpair: %v", err)
	}
	for _, fd := range p {
		if err := unix.SetNonblock(fd, true); err != nil {
			tb.Fatalf("SetNonblock: %v", err)
		}
	}
	return p[0], p[1]
}

// hookabBare: a bare Loop (no engine, no epoll): driverRead touches only
// driverReadBuf, and closeDriver on EOF or error, which does not happen here.
// ctlClosed refuses the EPOLL_CTL_MOD a Write's EAGAIN would issue.
func hookabBare(tb testing.TB, onRecv func([]byte)) (*Loop, *driverConn, int, int) {
	tb.Helper()
	local, peer := hookabPair(tb)
	dc := &driverConn{fd: local, onRecv: onRecv, onClose: func(error) {}}
	l := &Loop{driverConns: map[int]*driverConn{local: dc}, ctlClosed: true}
	l.hasDriverConns.Store(true)
	return l, dc, local, peer
}

func hookabDriverReadBare1B(t *testing.T) hookabStats {
	var reads, bytes atomic.Int64
	l, dc, local, peer := hookabBare(t, func(p []byte) { reads.Add(1); bytes.Add(int64(len(p))) })
	defer func() { _ = unix.Close(local); _ = unix.Close(peer) }()
	one := []byte{'x'}
	st := hookabStats{shape: "DriverReadBare1B", n: hookabBareN}
	t0, c0, m0 := st.begin()
	for i := 0; i < hookabBareN; i++ {
		if _, err := unix.Write(peer, one); err != nil {
			t.Fatalf("write: %v", err)
		}
		l.driverRead(dc)
	}
	st.end(t0, c0, m0)
	st.reads = reads.Load()
	if st.reads != hookabBareN || bytes.Load() != hookabBareN {
		st.errors++
		t.Errorf("DriverReadBare1B: %d reads, %d bytes, want %d of each", st.reads, bytes.Load(), hookabBareN)
	}
	return st
}

func hookabDriverReadBareFlood(t *testing.T) hookabStats {
	var reads, bytes atomic.Int64
	l, dc, local, peer := hookabBare(t, func(p []byte) { reads.Add(1); bytes.Add(int64(len(p))) })
	defer func() { _ = unix.Close(local); _ = unix.Close(peer) }()
	chunk := make([]byte, driverReadBufSize)
	st := hookabStats{shape: "DriverReadBareFlood", n: hookabFloodN}
	var sent int64
	t0, c0, m0 := st.begin()
	for i := 0; i < hookabFloodN; i++ {
		// Fill the socket to EAGAIN: a full buffer is what makes a read fill
		// driverReadBuf and the loop go on to the next of its reads.
		for {
			n, err := unix.Write(peer, chunk)
			if err != nil {
				if err == unix.EAGAIN {
					break
				}
				t.Fatalf("write: %v", err)
			}
			sent += int64(n)
		}
		for l.driverRead(dc) { // true: the budget ran out with bytes left
		}
	}
	st.end(t0, c0, m0)
	st.reads = reads.Load()
	if bytes.Load() != sent || st.reads == 0 {
		st.errors++
		t.Errorf("DriverReadBareFlood: onRecv got %d bytes in %d reads, sent %d", bytes.Load(), st.reads, sent)
	}
	return st
}

// hookabDriverCloseBare: one op registers a fresh socketpair end as a driver
// conn of a bare Loop, closes the peer and lets driverRead see the EOF, which
// takes closeDriver (the #1086 testHookCloseDriverDeleted seam sits after its
// EPOLL_CTL_DEL, once per op). The Loop has a real epoll descriptor, so the DEL
// is a real syscall (the kernel answers ENOENT: nothing was ever ADDed).
func hookabDriverCloseBare(t *testing.T) hookabStats {
	efd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Fatalf("epoll_create1: %v", err)
	}
	defer func() { _ = unix.Close(efd) }()
	l := &Loop{epollFD: efd, driverConns: map[int]*driverConn{}}
	var closed atomic.Int64
	st := hookabStats{shape: "DriverCloseBare", n: hookabCloseN}
	op := func() {
		local, peer := hookabPair(t)
		dc := &driverConn{fd: local, onRecv: func([]byte) {}, onClose: func(error) { closed.Add(1) }}
		l.driverMu.Lock()
		l.driverConns[local] = dc
		l.driverMu.Unlock()
		l.hasDriverConns.Store(true)
		_ = unix.Close(peer)
		l.driverRead(dc) // read returns 0: closeDriver
		_ = unix.Close(local)
	}
	t0, c0, m0 := st.begin()
	for i := 0; i < hookabCloseN; i++ {
		op()
	}
	st.end(t0, c0, m0)
	if closed.Load() != hookabCloseN {
		st.errors++
		t.Errorf("DriverCloseBare: onClose ran %d times, want %d", closed.Load(), hookabCloseN)
	}
	return st
}

// hookabEngine starts a Workers:2 epoll engine on a free loopback port with h
// as its handler. cfg.AsyncHandlers is async. The returned func stops it.
func hookabEngine(tb testing.TB, h stream.Handler, async bool) (*Engine, func()) {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("probe listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr:          addr,
		Protocol:      engine.HTTP1,
		Resources:     resource.Resources{Workers: 2},
		AsyncHandlers: async,
	}, h)
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Listen(ctx); close(done) }()
	for dl := time.Now().Add(15 * time.Second); time.Now().Before(dl) && (e.Addr() == nil || e.NumWorkers() == 0); {
		time.Sleep(2 * time.Millisecond)
	}
	if e.Addr() == nil {
		cancel()
		<-done
		tb.Fatalf("engine did not bind")
	}
	return e, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}
}

func hookabPingPong1B(t *testing.T) hookabStats {
	e, stop := hookabEngine(t, hookabNopHandler{}, false)
	defer stop()
	wl := e.WorkerLoop(0)
	local, peer := hookabPair(t)
	defer func() { _ = wl.UnregisterConn(local); _ = unix.Close(local); _ = unix.Close(peer) }()
	got := make(chan struct{}, 1)
	if err := wl.RegisterConn(local, func([]byte) {
		select {
		case got <- struct{}{}:
		default:
		}
	}, func(error) {}); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	one := []byte{'x'}
	st := hookabStats{shape: "PingPong1B", n: hookabPingN, lat: make([]time.Duration, 0, hookabPingN)}
	op := func() bool {
		if _, err := unix.Write(peer, one); err != nil {
			t.Fatalf("write: %v", err)
		}
		select {
		case <-got:
			return true
		case <-time.After(hookabOpTimeout):
			return false
		}
	}
	for i := 0; i < 200; i++ { // warm the loop, the channel and the timer
		op()
	}
	t0, c0, m0 := st.begin()
	for i := 0; i < hookabPingN; i++ {
		s := time.Now()
		if !op() {
			st.errors++
			continue
		}
		st.lat = append(st.lat, time.Since(s))
	}
	st.end(t0, c0, m0)
	if st.errors > 0 {
		t.Errorf("PingPong1B: %d pings not served within %v", st.errors, hookabOpTimeout)
	}
	return st
}

type hookabNopHandler struct{}

func (hookabNopHandler) HandleStream(context.Context, *stream.Stream) error { return nil }

// hookabHijackHandler: /hj hijacks, answers on the raw conn and closes it.
type hookabHijackHandler struct{}

func (hookabHijackHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	hj, ok := s.ResponseWriter.(stream.Hijacker)
	if !ok {
		return fmt.Errorf("response writer %T cannot hijack", s.ResponseWriter)
	}
	c, err := hj.Hijack(s)
	if err != nil {
		return err
	}
	_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nhj")
	return c.Close()
}

func hookabHijack(t *testing.T, shape string, async bool) hookabStats {
	e, stop := hookabEngine(t, hookabHijackHandler{}, async)
	defer stop()
	addr := e.Addr().String()
	st := hookabStats{shape: shape, n: hookabHijackN, lat: make([]time.Duration, 0, hookabHijackN)}
	req := []byte("GET /hj HTTP/1.1\r\nHost: x\r\n\r\n")
	op := func() bool {
		c, err := net.DialTimeout("tcp", addr, hookabOpTimeout)
		if err != nil {
			return false
		}
		defer func() { _ = c.Close() }()
		_ = c.SetDeadline(time.Now().Add(hookabOpTimeout))
		if _, err := c.Write(req); err != nil {
			return false
		}
		b, err := io.ReadAll(c)
		return err == nil && strings.HasSuffix(string(b), "\r\n\r\nhj")
	}
	for i := 0; i < 100; i++ {
		op()
	}
	promo0 := int64(e.Metrics().AsyncPromotedConns)
	t0, c0, m0 := st.begin()
	for i := 0; i < hookabHijackN; i++ {
		s := time.Now()
		if !op() {
			st.errors++
			continue
		}
		st.lat = append(st.lat, time.Since(s))
	}
	st.end(t0, c0, m0)
	st.promoted = int64(e.Metrics().AsyncPromotedConns) - promo0
	if st.errors > 0 {
		t.Errorf("%s: %d of %d hijack round trips failed", shape, st.errors, hookabHijackN)
	}
	return st
}

// TestHookAB is the timing-mode wrapper: every shape once per iteration, one
// HOOKAB line each. The order is fixed.
func TestHookAB(t *testing.T) {
	for _, run := range []func() hookabStats{
		func() hookabStats { return hookabDriverReadBare1B(t) },
		func() hookabStats { return hookabDriverReadBareFlood(t) },
		func() hookabStats { return hookabDriverCloseBare(t) },
		func() hookabStats { return hookabPingPong1B(t) },
		func() hookabStats { return hookabHijack(t, "HijackSync", false) },
		func() hookabStats { return hookabHijack(t, "HijackAsyncInline", true) },
	} {
		st := run()
		t.Log(st.line())
	}
}
