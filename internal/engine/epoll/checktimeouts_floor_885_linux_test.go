//go:build linux

package epoll

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#885 item 29: checkTimeouts is driven by the loop's iterations (every
// 32 epoll_wait returns when ReadHeaderTimeout is set, which is the default, or
// a conn is detached), so a busy loop swept thousands of times a second where
// the 25 ms timerfd promises 40 (measured: 5,400 to 6,200 a second in all on
// two loops with sixteen busy clients, 12.9% of a core on 8192 idle conns).
// The sweep now has a floor between two of them (sweepFloor), outside
// checkTimeouts, which the gate in the run loop tests.
//
// Two tests. The first counts the sweeps one busy loop makes in a window, under
// sixteen busy keep-alive clients and the default configuration; it fails on a
// loop whose gate has no floor. The second is the price of the floor and the
// guard against a floor set too high: with the timerfd off (ReadHeaderTimeout
// disabled) the gate is the only source of sweeps on a busy loop, and a
// detached conn's idle deadline (a WebSocket's, an SSE stream's) must still be
// reaped about when it expires.

// sweepProbe885 records the time of every sweep of the loop that serves addr.
type sweepProbe885 struct {
	addr string

	mu       sync.Mutex
	at       map[*Loop][]int64 // UnixNano of each sweep, by loop
	live     map[*Loop]int     // the conns a loop had at its last sweep
	timerFD  int               // the loop's timerfd as the sweep saw it
	sawLoop  bool
	detLoop  *Loop              // the loop the first detached conn is on
	detached chan *conn.H1State // the H1 state of each detached conn the sweep meets, once
	seen     map[*connState]bool
}

func (p *sweepProbe885) hook(l *Loop) {
	if l.cfg.Addr != p.addr {
		return
	}
	p.mu.Lock()
	p.at[l] = append(p.at[l], time.Now().UnixNano())
	p.live[l] = len(l.liveConns)
	p.timerFD, p.sawLoop = l.timerFD, true
	p.mu.Unlock()
	for _, cs := range l.liveConns {
		if h1 := cs.h1State; h1 != nil && h1.Detached.Load() && !p.seen[cs] {
			p.seen[cs] = true
			p.mu.Lock()
			p.detLoop = l
			p.mu.Unlock()
			select {
			case p.detached <- h1:
			default:
			}
		}
	}
}

// timerfd is the loop's timerfd as the last sweep saw it.
func (p *sweepProbe885) timerfd() (fd int, saw bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.timerFD, p.sawLoop
}

// window returns the sweep times of loop l (every loop's when l is nil) in
// [from, to].
func (p *sweepProbe885) window(l *Loop, from, to int64) []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var w []int64
	for k, at := range p.at {
		if l != nil && k != l {
			continue
		}
		for _, t := range at {
			if t >= from && t <= to {
				w = append(w, t)
			}
		}
	}
	slices.Sort(w)
	return w
}

// loops is the number of loops that swept, and how many conns the loop that
// holds the detached conn had at its last sweep.
func (p *sweepProbe885) loops() (n int, detLoop *Loop, detLive int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.at), p.detLoop, p.live[p.detLoop]
}

// floorHandler885 answers every request with a short body; /detach, on an
// async route, detaches the conn the way Context.Detach does on this engine
// (the WebSocket and SSE middlewares) and returns without a response.
type floorHandler885 struct{}

func (floorHandler885) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.Path == "/detach" {
		if s.OnDetach == nil {
			return io.ErrNoProgress
		}
		s.OnDetach()
		return nil
	}
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}}, []byte("ok"))
}
func (floorHandler885) RouteAsync(_, path string) bool { return path == "/detach" }
func (floorHandler885) HasAsyncRoutes() bool           { return true }

var _ stream.AsyncRouteResolver = floorHandler885{}

// startFloorEngine885 serves cfg on two loops (the least the engine accepts)
// and returns the address and the probe. The engine is stopped, and Listen waited for, at cleanup.
func startFloorEngine885(t *testing.T, cfg resource.Config) (string, *sweepProbe885) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback unavailable: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	cfg.Addr = addr
	cfg.Protocol = engine.HTTP1
	cfg.Resources = resource.Resources{Workers: 2}
	e, err := New(cfg, floorHandler885{})
	if err != nil {
		t.Fatalf("epoll engine: %v", err) // not a skip: a skip would take the witness out of CI silently
	}
	p := &sweepProbe885{addr: addr, at: map[*Loop][]int64{}, live: map[*Loop]int{},
		detached: make(chan *conn.H1State, 16), seen: map[*connState]bool{}}
	hook := p.hook
	testHookCheckTimeouts.Store(&hook)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("Listen did not return within 30 s of its context's cancellation")
		}
		testHookCheckTimeouts.Store(nil)
	})
	for deadline := time.Now().Add(8 * time.Second); ; {
		if c, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond); derr == nil {
			_ = c.Close()
			if e.NumWorkers() > 0 {
				break
			}
		}
		select {
		case err := <-done:
			t.Fatalf("epoll engine failed to start: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("engine did not start listening within 8s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return addr, p
}

// busy885 is a set of keep-alive clients, each sending the next request as
// soon as it has read the last response, until stop.
type busy885 struct {
	t      *testing.T
	addr   string
	served atomic.Int64
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// start adds n clients and waits for each to have been answered once.
func (b *busy885) start(n int) {
	b.t.Helper()
	started := make(chan struct{}, n)
	for range n {
		c, err := net.DialTimeout("tcp", b.addr, 3*time.Second)
		if err != nil {
			b.t.Fatalf("dial: %v", err)
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			first := true
			for {
				select {
				case <-b.stopCh:
					return
				default:
				}
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := c.Write([]byte("GET /ok HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
					return
				}
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				b.served.Add(1)
				if first {
					first = false
					started <- struct{}{}
				}
			}
		}()
	}
	for range n {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			b.t.Fatal("PREMISE: a busy client got no response within 10 s")
		}
	}
}

func (b *busy885) stop() { close(b.stopCh); b.wg.Wait() }

func newBusy885(t *testing.T, addr string) *busy885 {
	return &busy885{t: t, addr: addr, stopCh: make(chan struct{})}
}

// TestBusyLoopSweepsAtMostOnceAFloorPeriod885 is the sweep count: sixteen busy
// clients on one loop, the default configuration (ReadHeaderTimeout 10 s, so
// the timerfd is armed and the gate is every 32 iterations), a window of 1.5 s.
// The timerfd alone sweeps 40 times a second, so about 60 sweeps are due in the
// window and the floor must not let the gate add to that; a gate without the
// floor sweeps every 32 iterations, which on a busy loop is thousands a second.
func TestBusyLoopSweepsAtMostOnceAFloorPeriod885(t *testing.T) {
	addr, p := startFloorEngine885(t, resource.Config{})
	b := newBusy885(t, addr)
	b.start(16)
	defer b.stop()

	const span = 1500 * time.Millisecond
	from := time.Now()
	served0 := b.served.Load()
	time.Sleep(span)
	to := time.Now()
	reqs := b.served.Load() - served0
	sweeps := len(p.window(nil, from.UnixNano(), to.UnixNano()))
	nLoops, _, _ := p.loops()
	tfd, saw := p.timerfd()
	t.Logf("celeris885 item 29: %d sweeps in %v on %d loops under 16 busy clients (%d requests, %.0f req/s); timerFD=%d; sweepFloor=%v",
		sweeps, span, nLoops, reqs, float64(reqs)/span.Seconds(), tfd, sweepFloor)

	if !saw || tfd < 0 {
		t.Fatalf("PREMISE: the loop has no timerfd (saw loop %v, timerFD %d): the default ReadHeaderTimeout arms it", saw, tfd)
	}
	if reqs < 1000 {
		t.Fatalf("PREMISE: the loop was not busy: %d requests in %v", reqs, span)
	}
	// 40 a second a loop from the timerfd; slack for jitter and a sweep at
	// each edge of the window.
	if lim := nLoops * (int(span/sweepFloor) + 20); sweeps > lim {
		t.Errorf("%d sweeps in %v on %d loops under load, want at most %d: a busy loop sweeps every %v at most (the timerfd period), not every 32 iterations",
			sweeps, span, nLoops, lim, sweepFloor)
	}
	if sweeps < 20*nLoops {
		t.Errorf("only %d sweeps in %v on %d loops: the floor starved the sweep", sweeps, span, nLoops)
	}
}

// TestDetachedIdleDeadlineIsReapedPromptlyUnderLoad885 is the price of the
// floor. ReadHeaderTimeout is disabled, so there is no timerfd and the gate
// (every 32 iterations while a conn is detached) is the only sweep on a busy
// loop. Sixteen clients keep the loop busy; a detached conn (a WebSocket's,
// SSE's) gets an idle deadline 200 ms ahead and must be reaped (within 400 ms
// of it, a sanity bound). The median gap between two sweeps is the structural
// bound, and does not depend on where in a gap the deadline falls: a floor of
// 250 ms moves every gap past it (the control of that mutant), where a floor
// of the timerfd's 25 ms leaves the gaps at the time of 32 iterations.
func TestDetachedIdleDeadlineIsReapedPromptlyUnderLoad885(t *testing.T) {
	addr, p := startFloorEngine885(t, resource.Config{ReadHeaderTimeout: -1, AsyncHandlers: true})
	dc, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dc.Close() }()
	if _, err := dc.Write([]byte("GET /detach HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	var h1 *conn.H1State
	select {
	case h1 = <-p.detached:
	case <-time.After(10 * time.Second):
		t.Fatal("PREMISE: no sweep met the detached conn within 10 s")
	}
	b := newBusy885(t, addr)
	b.start(16)
	defer b.stop()
	// The gap is measured on the loop that holds the detached conn, so that
	// loop must be busy: clients are added until it has at least nine conns
	// besides the detached one (the hash of the 4-tuple picks the loop).
	var detLoop *Loop
	for added := 0; ; {
		time.Sleep(100 * time.Millisecond)
		var live int
		if _, detLoop, live = p.loops(); detLoop != nil && live >= 10 {
			break
		}
		if added += 4; added > 48 {
			t.Fatalf("PREMISE: the loop with the detached conn has %d conns after %d more clients", live, added)
		}
		b.start(4)
	}

	from := time.Now()
	served0 := b.served.Load()
	const lead = 200 * time.Millisecond
	deadline := time.Now().Add(lead)
	h1.IdleDeadlineNs.Store(deadline.UnixNano())
	// The reap closes the socket: the detached client reads EOF.
	_ = dc.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, rerr := dc.Read(make([]byte, 1))
	reaped := time.Now()
	if rerr == nil {
		t.Fatal("the detached conn read data, want EOF at its reap")
	}
	if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
		t.Fatalf("the detached conn was not reaped within 5 s of an idle deadline %v ahead", lead)
	}
	time.Sleep(600 * time.Millisecond) // more sweeps to measure the gaps
	to := time.Now()
	reqs := b.served.Load() - served0

	w := p.window(detLoop, from.UnixNano(), to.UnixNano())
	var gaps []time.Duration
	for i := 1; i < len(w); i++ {
		gaps = append(gaps, time.Duration(w[i]-w[i-1]))
	}
	slices.Sort(gaps)
	// The median, not the maximum: one gap can be a GC pause or a descheduled
	// thread, and a gap is at least the time of the 32 iterations of a gate
	// stride, which a -race run on a loaded host stretches to tens of
	// milliseconds, but a floor set too high moves every gap.
	var medGap, maxGap time.Duration
	if n := len(gaps); n > 0 {
		medGap, maxGap = gaps[n/2], gaps[n-1]
	}
	late := reaped.Sub(deadline)
	tfd, saw := p.timerfd()
	t.Logf("celeris885 item 29: detached conn reaped %v after its deadline; %d sweeps of its loop in %v, gaps median %v max %v, %d requests in all; timerFD=%d",
		late.Round(time.Millisecond), len(w), to.Sub(from).Round(time.Millisecond), medGap.Round(time.Millisecond), maxGap.Round(time.Millisecond), reqs, tfd)

	if !saw || tfd >= 0 {
		t.Fatalf("PREMISE: the loop has a timerfd (%d, saw %v): ReadHeaderTimeout -1 disables it, and without that the gate is not the only sweep", tfd, saw)
	}
	if reqs < 1000 {
		t.Fatalf("PREMISE: the loop was not busy: %d requests in %v", reqs, to.Sub(from))
	}
	if len(w) < 2 {
		t.Fatalf("PREMISE: %d sweeps of the detached conn's loop in %v", len(w), to.Sub(from))
	}
	// A sanity bound on the one reap: where in a gap the deadline falls decides
	// it, and a gap can be a few hundred milliseconds on a loaded host.
	if late > 2*lead {
		t.Errorf("the detached conn was reaped %v after its idle deadline, want within %v", late.Round(time.Millisecond), 2*lead)
	}
	if medGap > 200*time.Millisecond {
		t.Errorf("the median gap between two sweeps of a busy loop was %v (max %v), want at most 200 ms (floor %v): the sweep floor is too high",
			medGap.Round(time.Millisecond), maxGap.Round(time.Millisecond), sweepFloor)
	}
}
