//go:build linux

package celeris_test

import (
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/internal/probe"
)

// Tests for celeris#974. Before protocol detection has decided, the io_uring
// engine kept every byte it received in cs.detectAccum and, when the bytes
// were not a protocol it serves (ErrUnknownProtocol), only re-armed the
// recv. A client that sent "PRI " (the start of the HTTP/2 preface, enough to
// keep detection undecided) and then any amount of garbage made the server
// hold all of it with the connection open, and nothing reaped the connection:
// every recv refreshed the activity stamp the idle and read timeouts use, and
// the header deadline is armed only once a protocol is detected. A client
// that sent bytes the detector never recognises from the first one was held
// open too (nothing was kept, but the connection stayed).
//
// Two properties are tested apart, so that a fix of one cannot hide the other:
// the connection is closed, and the heap does not grow with what was sent.
// Then the pre-detection deadline: a client that sends "GE" and stalls, one
// that dribbles the HTTP/2 preface, and one that sends nothing, must be
// reaped by ReadHeaderTimeout, which counts from accept.

// garbageChunk974 is one write of the flood; the flood is garbageChunks974 of
// them. The base build keeps all of it (the E5 probe measured +146 MiB of heap
// for 128 MiB sent).
const (
	garbageChunks974     = 96
	garbageChunkSize     = 1 << 20
	heapBound974         = 12 << 20
	closeWithin974       = 5 * time.Second
	readHeaderTimeout974 = 300 * time.Millisecond
)

// engines974: the "-async" arms run with AsyncHandlers on, where a conn has a
// detachMu from accept and closeConn takes the detached-close path.
//
// The "-mshot" arm opts into multishot recv with a provided-buffer ring
// (CELERIS_IOURING_MULTISHOT_RECV=1). That is off by default, so without it no
// test here runs the provided-buffer paths of the detect branch, which return
// the ring buffer on every exit; "tier=high ... provided_buffers=true" in the
// engine's start-up log is the tier's capability, not the ring being in use.
type engine974 struct {
	name  string
	eng   celeris.EngineType
	async bool
	mshot bool
}

var engines974 = []engine974{{"std", celeris.Std, false, false}, {"epoll", celeris.Epoll, false, false}, {"io_uring", celeris.IOUring, false, false},
	{"epoll-async", celeris.Epoll, true, false}, {"io_uring-async", celeris.IOUring, true, false}, {"io_uring-mshot", celeris.IOUring, false, true}}

// env974 applies the arm's environment; call it before the server starts.
func (e engine974) env974(t *testing.T) {
	t.Helper()
	if e.mshot {
		t.Setenv("CELERIS_IOURING_MULTISHOT_RECV", "1")
	}
}

// deadlineEngines974 leaves epoll out: its first-bytes state is not under any
// header deadline either (it reaps a silent or stalled undetected connection
// only at ReadTimeout or IdleTimeout), which this change does not touch. It is
// listed in the PR body of celeris#974 and tracked in the epoll polish
// checklist (celeris#885, "no header deadline before protocol detection");
// add epoll here when that is fixed.
var deadlineEngines974 = []engine974{{"std", celeris.Std, false, false}, {"io_uring", celeris.IOUring, false, false}, {"io_uring-async", celeris.IOUring, true, false}}

func heapAfterGC974() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// heapPeak974 samples HeapAlloc (no forced GC) every 10 ms until stop is
// called, and returns the largest value seen. The bytes a server holds for as
// long as it keeps a connection are freed once it closes it, so the heap after
// the connection is gone says nothing about them: a server that closes late is
// caught only by sampling while it holds them.
func heapPeak974() (stop func() uint64) {
	var peak atomic.Uint64
	done := make(chan struct{})
	finished := make(chan struct{})
	sample := func() {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if m.HeapAlloc > peak.Load() {
			peak.Store(m.HeapAlloc)
		}
	}
	go func() {
		defer close(finished)
		for {
			sample()
			select {
			case <-done:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	return func() uint64 {
		close(done)
		<-finished
		sample()
		return peak.Load()
	}
}

// rssMiB974 is the resident set of the test process (the server is in it).
func rssMiB974() int64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return -1
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return -1
	}
	pages, _ := strconv.ParseInt(f[1], 10, 64)
	return pages * int64(os.Getpagesize()) >> 20
}

// watchClose974 reads c until it fails. A timeout is not a close. The returned
// channel yields the error that ended the read, and whether it was a close
// (EOF or a reset) rather than the read deadline.
type closeResult974 struct {
	at     time.Time
	err    error
	closed bool
	nbytes int64
}

// watcher974 is a reader on c; done is closed once r is set.
type watcher974 struct {
	done chan struct{}
	r    closeResult974
}

func (w *watcher974) wait() closeResult974 {
	<-w.done
	return w.r
}

func (w *watcher974) finished() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

func watchClose974(c net.Conn, limit time.Duration) *watcher974 {
	w := &watcher974{done: make(chan struct{})}
	go func() {
		defer close(w.done)
		_ = c.SetReadDeadline(time.Now().Add(limit))
		buf := make([]byte, 4096)
		var n int64
		for {
			k, err := c.Read(buf)
			n += int64(k)
			if err != nil {
				var ne net.Error
				timeout := errors.As(err, &ne) && ne.Timeout()
				w.r = closeResult974{at: time.Now(), err: err, closed: !timeout, nbytes: n}
				return
			}
		}
	}()
	return w
}

func startAuto974(t *testing.T, eng celeris.EngineType, extra func(*celeris.Config)) string {
	t.Helper()
	cfg := celeris.Config{Engine: eng, Protocol: celeris.Auto}
	if extra != nil {
		extra(&cfg)
	}
	return startServerConfig761(t, cfg, func(*celeris.Server) {})
}

// TestUnrecognisedBytesAreNotHeld974: the connection that sends a prefix and
// then a flood is closed and the server's heap does not grow with the flood.
func TestUnrecognisedBytesAreNotHeld974(t *testing.T) {
	leads := []struct{ name, lead string }{
		{"PRI-", "PRI "},
		{"PRI-line", "PRI * HTTP/2.0\r\n"},
		{"PRI-23", "PRI * HTTP/2.0\r\n\r\nSM\r\n\r"},
		{"garbage-from-byte-0", "\x16\x03\x01\x02"},
	}
	t.Logf("kernel %s, io_uring tier %v (CELERIS_MAX_IOURING_TIER=%q)", probe.Probe().KernelVersion,
		probe.Probe().IOUringTier, os.Getenv("CELERIS_MAX_IOURING_TIER"))
	// Protocol Auto takes every lead. Protocol H2C with EnableH2Upgrade runs
	// the same detection (celeris#974 review), so it is pinned with the
	// "PRI " lead on every engine that has the state (std has none: net/http
	// reads the request line).
	h2c := true
	type floodCase struct {
		e     engine974
		proto string
		set   func(*celeris.Config)
		l     struct{ name, lead string }
	}
	var cases []floodCase
	for _, e := range engines974 {
		for _, l := range leads {
			cases = append(cases, floodCase{e, "auto", nil, l})
		}
		if e.eng != celeris.Std {
			cases = append(cases, floodCase{e, "h2c-upgrade", func(c *celeris.Config) { c.Protocol = celeris.H2C; c.EnableH2Upgrade = &h2c }, leads[0]})
		}
	}
	for _, fc := range cases {
		e, l := fc.e, fc.l
		t.Run(e.name+"/"+fc.proto+"/"+l.name, func(t *testing.T) {
			e.env974(t)
			// The header deadline is out of reach: only the server's own
			// verdict on the bytes can close this connection, so the
			// deadline cannot stand in for it.
			addr := startAuto974(t, e.eng, func(c *celeris.Config) {
				c.AsyncHandlers = e.async
				if fc.set != nil {
					fc.set(c)
				}
				c.ReadHeaderTimeout = 5 * time.Minute
				c.ReadTimeout = 5 * time.Minute
				c.IdleTimeout = 5 * time.Minute
			})
			warm, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(warm, "GET /ping HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
			_, _ = io.Copy(io.Discard, warm)
			_ = warm.Close()

			h0, rss0 := heapAfterGC974(), rssMiB974()
			stopPeak := heapPeak974()
			c, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			res := watchClose974(c, closeWithin974)
			if _, err := io.WriteString(c, l.lead); err != nil {
				t.Fatal(err)
			}
			time.Sleep(100 * time.Millisecond)
			chunk := []byte(strings.Repeat("Z", garbageChunkSize))
			sent := 0
			var werr error
			for ; sent < garbageChunks974; sent++ {
				_ = c.SetWriteDeadline(time.Now().Add(3 * time.Second))
				if _, werr = c.Write(chunk); werr != nil {
					break
				}
			}
			// The server has had the whole flood (or has closed): the
			// close must be visible now, not at a timeout.
			r := res.wait()
			time.Sleep(300 * time.Millisecond)
			peak := stopPeak()
			h1, rss1 := heapAfterGC974(), rssMiB974()
			delta := int64(h1) - int64(h0)
			peakDelta := int64(peak) - int64(h0)
			t.Logf("%s/%s: sent %d MiB (write error: %v), closed=%v (%v), heap %d -> %d KiB (delta %d KiB, peak delta %d KiB), rss %d -> %d MiB",
				e.name, fc.proto+"/"+l.name, sent, werr, r.closed, r.err, h0>>10, h1>>10, delta>>10, peakDelta>>10, rss0, rss1)
			if !r.closed {
				t.Errorf("the server did not close a connection that sent %q and then %d MiB of unrecognised bytes (read ended with %v)",
					l.lead, sent, r.err)
			}
			if delta > heapBound974 {
				t.Errorf("heap grew by %d MiB (bound %d MiB) for %d MiB sent after %q: the server holds the bytes",
					delta>>20, heapBound974>>20, sent, l.lead)
			}
			// std is the control for the close and the deadline cases, not
			// for the peak: net/http reads a request line up to
			// MaxHeaderBytes (16 MiB by default) before it answers. The
			// peak is HeapAlloc without a GC, so it counts the garbage of
			// that read: 21 to 87 MiB measured on std, while its heap
			// after a GC does not move (see the PR body of celeris#974).
			// The engines under test hold a few bytes.
			if e.eng != celeris.Std && peakDelta > heapBound974 {
				t.Errorf("heap peaked %d MiB over its start (bound %d MiB) while %d MiB were sent after %q: the server held the bytes before it closed",
					peakDelta>>20, heapBound974>>20, sent, l.lead)
			}
		})
	}
}

// TestStalledPreDetectionConnIsReaped974: ReadHeaderTimeout counts from
// accept on a connection whose protocol is not detected yet. ReadTimeout and
// IdleTimeout are far longer, so only the header deadline can reap these.
func TestStalledPreDetectionConnIsReaped974(t *testing.T) {
	h2c := true
	protos := []struct {
		name string
		set  func(*celeris.Config)
	}{
		{"auto", nil},
		{"h2c-upgrade", func(c *celeris.Config) { c.Protocol = celeris.H2C; c.EnableH2Upgrade = &h2c }},
	}
	cases := []struct {
		name string
		run  func(t *testing.T, c net.Conn, res *watcher974) (sentAll bool)
	}{
		{"silent", func(t *testing.T, c net.Conn, res *watcher974) bool { return false }},
		{"GE-then-stall", func(t *testing.T, c net.Conn, res *watcher974) bool {
			_, _ = io.WriteString(c, "GE")
			return false
		}},
		// The HTTP/2 preface, one byte per 200 ms from byte 4: it would
		// complete after 4 s, long after ReadHeaderTimeout.
		{"dribbled-preface", func(t *testing.T, c net.Conn, res *watcher974) bool {
			const preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
			_, _ = io.WriteString(c, preface[:4])
			for i := 4; i < len(preface); i++ {
				time.Sleep(200 * time.Millisecond)
				if res.finished() {
					return false
				}
				if _, err := c.Write([]byte{preface[i]}); err != nil {
					return false
				}
			}
			return true
		}},
	}
	for _, e := range deadlineEngines974 {
		for _, p := range protos {
			for _, cs := range cases {
				if p.set != nil && e.eng == celeris.Std {
					continue // std has no pre-detection state (net/http reads the request line)
				}
				t.Run(e.name+"/"+p.name+"/"+cs.name, func(t *testing.T) {
					e.env974(t)
					addr := startAuto974(t, e.eng, func(c *celeris.Config) {
						c.AsyncHandlers = e.async
						c.ReadHeaderTimeout = readHeaderTimeout974
						c.ReadTimeout = 5 * time.Minute
						c.IdleTimeout = 5 * time.Minute
						if p.set != nil {
							p.set(c)
						}
					})
					c, err := net.DialTimeout("tcp", addr, 5*time.Second)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = c.Close() }()
					start := time.Now()
					res := watchClose974(c, closeWithin974)
					sentAll := cs.run(t, c, res)
					r := res.wait()
					t.Logf("%s: closed=%v (%v) after %v, whole preface sent=%v, bytes read back=%d",
						t.Name(), r.closed, r.err, r.at.Sub(start).Round(10*time.Millisecond), sentAll, r.nbytes)
					if !r.closed {
						t.Errorf("a connection that stalls before its protocol is detected was not closed within %v "+
							"(ReadHeaderTimeout %v): read ended with %v", closeWithin974, readHeaderTimeout974, r.err)
					}
					if sentAll {
						t.Errorf("the whole HTTP/2 preface was sent over %v at ReadHeaderTimeout %v: "+
							"the header deadline does not count from accept", r.at.Sub(start), readHeaderTimeout974)
					}
				})
			}
		}
	}
}

// TestSplitFirstSegmentIsServed974 guards the other half of the change: the
// bytes held across recvs, and the rest of the recv that decides detection,
// must still reach the protocol handler whole. The deciding recv here carries
// more than detection needs (the whole request, and the preface plus a
// SETTINGS frame), so a fix that handed on only what Detect looked at, or
// only the held bytes, loses the request or the frame. std is the control;
// epoll's split segments are celeris#870.
func TestSplitFirstSegmentIsServed974(t *testing.T) {
	engines := []engine974{{"std", celeris.Std, false, false}, {"io_uring", celeris.IOUring, false, false}, {"io_uring-async", celeris.IOUring, true, false}, {"io_uring-mshot", celeris.IOUring, false, true}}
	h1 := []struct{ name, first, rest string }{
		{"GE", "GE", "T /ping HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"},
		{"G", "G", "ET /ping HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"},
		{"GET-", "GET ", "/ping HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"},
	}
	const preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	emptySettings := "\x00\x00\x00\x04\x00\x00\x00\x00\x00"
	h2 := []struct{ name, first, rest string }{
		{"PRI-", preface[:4], preface[4:] + emptySettings},
		{"preface-10", preface[:10], preface[10:] + emptySettings},
		{"preface-23", preface[:23], preface[23:] + emptySettings},
	}
	for _, e := range engines {
		for _, c := range h1 {
			t.Run(e.name+"/h1/"+c.name, func(t *testing.T) {
				e.env974(t)
				addr := startAuto974(t, e.eng, func(c *celeris.Config) { c.AsyncHandlers = e.async })
				conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conn.Close() }()
				_, _ = io.WriteString(conn, c.first)
				time.Sleep(100 * time.Millisecond)
				_, _ = io.WriteString(conn, c.rest)
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				b, _ := io.ReadAll(conn)
				if !strings.HasPrefix(string(b), "HTTP/1.1 200") || !strings.HasSuffix(string(b), "ok") {
					t.Errorf("split request %q + %q: got %q, want a 200 answer", c.first, c.rest, b)
				}
			})
		}
		for _, c := range h2 {
			t.Run(e.name+"/h2c/"+c.name, func(t *testing.T) {
				e.env974(t)
				addr := startAuto974(t, e.eng, func(c *celeris.Config) { c.AsyncHandlers = e.async })
				conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conn.Close() }()
				_, _ = io.WriteString(conn, c.first)
				time.Sleep(100 * time.Millisecond)
				_, _ = io.WriteString(conn, c.rest)
				// The server answers with its SETTINGS, then acknowledges
				// ours: a SETTINGS frame with the ACK flag.
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				hdr := make([]byte, 9)
				for {
					if _, err := io.ReadFull(conn, hdr); err != nil {
						t.Fatalf("split preface %q + rest: no SETTINGS ACK from the server: %v", c.first, err)
					}
					n := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
					if _, err := io.CopyN(io.Discard, conn, int64(n)); err != nil {
						t.Fatalf("frame body: %v", err)
					}
					if hdr[3] == 0x4 && hdr[4]&0x1 != 0 {
						return
					}
				}
			})
		}
	}
}

// TestUnknownProtocolReturnsItsRingBuffer974: with multishot recv the first
// bytes of a conn arrive in a buffer of the worker's provided-buffer ring, and
// the buffer has to go back to the ring on every exit of the detect branch,
// the close on an unrecognised protocol included. One that is dropped is gone
// for good (celeris#974 review). The ring of a worker holds at least 1024
// buffers (bufRingCountMin in internal/engine/iouring, the size this test
// pins with CELERIS_IOURING_PBUF_COUNT), so a worker that closes more
// unrecognised first segments than that, without returning the buffers, has an
// empty ring: the next conn gets ENOBUFS on every recv and is never answered.
// The engine needs two workers or more (Config.Workers is 2 at the least; a
// host with a low memlock limit may cap that to one), so the test sends three
// rings' worth of conns: whatever the split, every worker is past its ring.
// The last conn is a plain GET.
func TestUnknownProtocolReturnsItsRingBuffer974(t *testing.T) {
	const (
		ringSize = 1024 // bufRingCountMin
		badConns = 3 * ringSize
		parallel = 256
	)
	t.Setenv("CELERIS_IOURING_PBUF_COUNT", strconv.Itoa(ringSize))
	engine974{"io_uring-mshot", celeris.IOUring, false, true}.env974(t)
	addr := startAuto974(t, celeris.IOUring, func(c *celeris.Config) { c.Workers = 2 })

	var closed atomic.Int64
	sem := make(chan struct{}, parallel)
	done := make(chan struct{}, badConns)
	for i := 0; i < badConns; i++ {
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; done <- struct{}{} }()
			c, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			_, _ = io.WriteString(c, "\x16\x03\x01\x02")
			if r := watchClose974(c, 2*time.Second).wait(); r.closed {
				closed.Add(1)
			}
		}()
	}
	for i := 0; i < badConns; i++ {
		<-done
	}
	t.Logf("%d of %d conns that opened with garbage were closed by the server", closed.Load(), badConns)
	if closed.Load() != badConns {
		t.Errorf("only %d of %d conns that opened with garbage were closed: a worker's ring ran out of buffers", closed.Load(), badConns)
	}

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "GET /ping HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, _ := io.ReadAll(conn)
	if !strings.HasPrefix(string(b), "HTTP/1.1 200") || !strings.HasSuffix(string(b), "ok") {
		t.Errorf("a GET after %d conns that opened with garbage got %q, want a 200 answer", badConns, b)
	}
}
