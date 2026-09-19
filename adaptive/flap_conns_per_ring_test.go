//go:build linux

package adaptive

// celeris#657 face 2, T4: the CI-shape test for the io_uring -> epoll hand-off loss (celeris#681 round 2 commits it
// from the step-0 overlay the campaign measured: base 5b2e83b FAIL 8/8, fix PASS 8/8).
//
// The sync loss at a revert depends on linked SEND->RECV chains PER RING, not on the io_uring worker count, and
// CI's adaptive job raises memlock to unlimited, which gives io_uring its full worker count and so few conns per
// ring that the loss does not show. T4 pins the variable instead of the environment: Resources.Workers=2 fixes
// both engines at 2 workers, so 256 keep-alive conns are 128 per ring after a promotion, whatever the memlock.
//
// Shape (DECISION.md step 0; red-team.md section 7, T4): controller frozen, THREE promote/revert cycles under
// continuous back-to-back load, 2.5 s after each switch (more than a 2 s client read deadline, so a request lost
// at switch k is counted before switch k+1). It requires ZERO client errors: a client error here is a request the
// server never answered.
//
// It also requires, from the adaptive engine's Metrics() (both sub-engines summed; celeris#681 R4), the two
// celeris#657 loss witnesses at 0 -- W1, a request read by a recv that outlived its hand-off
// (StaleRecvDataTransplanted + StaleRecvDataUnattributed), and W2, a hand-off made with an op in flight
// (TransplantHandoffInFlight) -- and the three hand-off counters that must stay 0 (TransplantDoubleClaim,
// TransplantHoldRescued, TransplantReapFailed). And every switch must have moved the connections: the engine
// switched away from detached at least all of them and holds none, and the engine switched to adopted at least
// all of them and holds all of them. A switch that moved nothing would pass every loss check vacuously.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

func TestFlapConnsPerRing(t *testing.T) { s0FlapConnsPerRing(t, 3) }

func s0FlapConnsPerRing(t *testing.T, cycles int) {
	if testing.Short() {
		t.Skip("integration")
	}
	const (
		workers = 2
		perRing = 128
		conns   = workers * perRing
		dwell   = 2500 * time.Millisecond
	)
	// Two io_uring workers need 24 MiB of RLIMIT_MEMLOCK (12 MiB each, engine/iouring minMemlockPerWorker). Below
	// that io_uring caps itself to one ring and the PREMISE check below fails on the environment, not the engine.
	// The adaptive CI job raises memlock and sets CELERIS_REQUIRE_UPSWITCH=1, which turns this skip, and s0Bind's,
	// into a failure.
	if m := maxWorkersForMemlock(); m >= 0 && m < workers {
		s0SkipUnlessRequired(t, "RLIMIT_MEMLOCK funds %d io_uring worker(s), T4 needs %d", m, workers)
	}
	e, addr, stop := s0Bind(t, resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1,
		Resources: resource.Resources{Workers: workers}}, respHandler{})
	defer stop()

	stopLoad := make(chan struct{})
	var okCount, errCount atomic.Int64
	var cen s0Census
	wg := s0Drive(addr, conns, stopLoad, &okCount, &errCount, &cen)
	time.Sleep(500 * time.Millisecond)
	t.Logf("S0T4 START cycles=%d conns=%d epoll=%d ok=%d err=%d", cycles, conns,
		e.primary.Metrics().ActiveConnections, okCount.Load(), errCount.Load())

	// side is one sub-engine's metrics: io_uring (secondary) or epoll (primary). Before the first promotion the
	// lazy standby is not built yet, and reads as zero.
	side := func(ioUring bool) engine.EngineMetrics {
		en := e.primary
		if ioUring {
			en = e.secondary
		}
		if en == nil {
			return engine.EngineMetrics{}
		}
		return en.Metrics()
	}
	for sw := 1; sw <= 2*cycles; sw++ {
		dir, toIOUring := "promote", sw%2 == 1
		if !toIOUring {
			dir = "revert"
		}
		errBefore := errCount.Load()
		srcBefore, dstBefore := side(!toIOUring), side(toIOUring)
		t0 := time.Now()
		e.ForceSwitch()
		took := time.Since(t0)
		time.Sleep(dwell)
		pm, sm := e.primary.Metrics(), e.secondary.Metrics()
		src, dst := side(!toIOUring), side(toIOUring)
		moved := src.TransplantDetached - srcBefore.TransplantDetached
		adopted := dst.TransplantAdopted - dstBefore.TransplantAdopted
		t.Logf("S0T4 SWITCH n=%d dir=%s took_ms=%d epoll=%d io_uring=%d wE=%d wI=%d moved=%d adopted=%d ok=%d err=%d err_this=%d census=%s",
			sw, dir, took.Milliseconds(), pm.ActiveConnections, sm.ActiveConnections, pm.Workers, sm.Workers,
			moved, adopted, okCount.Load(), errCount.Load(), errCount.Load()-errBefore, cen.String())
		if moved < conns || adopted < conns || src.ActiveConnections != 0 || dst.ActiveConnections != conns {
			t.Logf("S0T4 MOVE: switch %d (%s) detached %d and adopted %d of %d conns, and left %d on the engine "+
				"switched away from and %d on the one switched to: want every conn moved", sw, dir, moved, adopted,
				conns, src.ActiveConnections, dst.ActiveConnections)
		}
	}
	close(stopLoad)
	wg.Wait()
	ew, iw := e.primary.Metrics().Workers, e.secondary.Metrics().Workers
	m := e.Metrics()
	w1 := m.StaleRecvDataTransplanted + m.StaleRecvDataUnattributed
	t.Logf("S0T4 RESULT cycles=%d conns=%d wE=%d wI=%d conns_per_ring=%d ok=%d err=%d w1=%d w2=%d doubleclaim=%d "+
		"holdrescued=%d reapfailed=%d held=%d reaps=%d census=%s",
		cycles, conns, ew, iw, conns/max(iw, 1), okCount.Load(), errCount.Load(), w1, m.TransplantHandoffInFlight,
		m.TransplantDoubleClaim, m.TransplantHoldRescued, m.TransplantReapFailed, m.TransplantHeld, m.TransplantReaps,
		cen.String())
	if ew != workers || iw != workers {
		t.Logf("S0T4 PREMISE: want %d workers on both engines, got epoll=%d io_uring=%d", workers, ew, iw)
	}
	if n := errCount.Load(); n != 0 {
		t.Logf("S0T4 LOSS: %d of %d keep-alive clients lost a request across %d promote/revert cycles at %d conns per ring (census %s)",
			n, conns, cycles, conns/max(iw, 1), cen.String())
	}
	if w1 != 0 {
		t.Logf("S0T4 W1: %d requests were read by a recv that outlived its hand-off (StaleRecvDataTransplanted=%d "+
			"StaleRecvDataUnattributed=%d), want 0", w1, m.StaleRecvDataTransplanted, m.StaleRecvDataUnattributed)
	}
	if n := m.TransplantHandoffInFlight; n != 0 {
		t.Logf("S0T4 W2: %d hand-offs were made with an op in flight (TransplantHandoffInFlight), want 0", n)
	}
	if n := m.TransplantDoubleClaim; n != 0 {
		t.Logf("S0T4 DOUBLECLAIM: TransplantDoubleClaim = %d, want 0", n)
	}
	if n := m.TransplantHoldRescued; n != 0 {
		t.Logf("S0T4 HOLDRESCUED: TransplantHoldRescued = %d, want 0", n)
	}
	if n := m.TransplantReapFailed; n != 0 {
		t.Logf("S0T4 REAPFAILED: TransplantReapFailed = %d, want 0", n)
	}
}

// s0SkipUnlessRequired skips T4 for an environment that cannot run it, and fails it instead when
// CELERIS_REQUIRE_UPSWITCH=1, as the adaptive CI job sets it: that job must not go green by skipping T4.
func s0SkipUnlessRequired(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("CELERIS_REQUIRE_UPSWITCH") == "1" {
		t.Fatal(msg + " -- CELERIS_REQUIRE_UPSWITCH=1 forbids skipping")
	}
	t.Skip(msg)
}

// The client driver and bind helper of TestFlapConnsPerRing (from the celeris#657 step-0 overlay). The driver keeps
// driveKeepAlive's shape (write a request, read the response with a 2 s deadline, stop at the first error, never
// redial), so an error count is the number of connections that lost a request. It adds a per-class census that
// never saturates. The identifiers keep their s0 prefix.

type s0Census struct {
	mu sync.Mutex
	m  map[string]int
}

func (c *s0Census) add(kind string, err error) {
	cls := "other"
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		cls = "timeout"
	case errors.Is(err, syscall.ECONNRESET):
		cls = "reset"
	case errors.Is(err, syscall.EPIPE):
		cls = "epipe"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		cls = "eof"
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]int{}
	}
	c.m[kind+":"+cls]++
	c.mu.Unlock()
}

func (c *s0Census) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(c.m))
	for k := range c.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%d,", k, c.m[k])
	}
	return strings.TrimSuffix(b.String(), ",")
}

// s0Drive opens conns keep-alive HTTP/1.1 connections that loop write-request / read-response back to back
// until stop is closed.
func s0Drive(addr string, conns int, stop <-chan struct{}, ok, errc *atomic.Int64, cen *s0Census) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
			if derr != nil {
				errc.Add(1)
				cen.add("dial", derr)
				return
			}
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, werr := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); werr != nil {
					errc.Add(1)
					cen.add("write", werr)
					return
				}
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				resp, rerr := http.ReadResponse(br, nil)
				if rerr != nil {
					errc.Add(1)
					cen.add("read", rerr)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				ok.Add(1)
			}
		}()
	}
	return &wg
}

// s0Bind builds an adaptive engine from cfg, disables the switch cooldown, FREEZES the controller (so the
// only switches are the test's ForceSwitch calls; performSwitch ignores the freeze), starts Listen and waits
// for the bind. Where io_uring or the adaptive engine is unavailable it skips, or fails under
// CELERIS_REQUIRE_UPSWITCH=1 (s0SkipUnlessRequired).
func s0Bind(t *testing.T, cfg resource.Config, h stream.Handler) (*Engine, string, func()) {
	t.Helper()
	if !probe.Probe().IOUringTier.Available() {
		s0SkipUnlessRequired(t, "io_uring unavailable: needs both sub-engines")
	}
	e, err := New(cfg, h, nil)
	if err != nil {
		s0SkipUnlessRequired(t, "adaptive.New unsupported here: %v", err)
	}
	e.ctrl.cooldown = 0
	e.FreezeSwitching()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	for dl := time.Now().Add(3 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		cancel()
		<-done
		t.Fatal("adaptive engine never bound")
	}
	return e, e.Addr().String(), func() { cancel(); <-done }
}
