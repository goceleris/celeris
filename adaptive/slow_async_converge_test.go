//go:build linux

package adaptive

// celeris#657 P8 (A1): a slow async handler's connection must follow a switch.
//
// A busy async connection on the outgoing epoll engine is movable only while its dispatch goroutine is parked
// with an empty input buffer, which for a back-to-back client is the short gap between a response and the next
// request. A periodic sweep finds it movable with a probability equal to that idle fraction, which is small for
// a slow handler, and slow handlers are what async dispatch exists for. The assertion (DECISION.md step 0 and
// step 3): after a promotion, the outgoing epoll engine holds <= 2 connections within 1 s.
//
// Shape: 64 back-to-back keep-alive clients, an async route whose handler sleeps 5 ms, controller frozen,
// 1 s of load on epoll (every conn promoted to its dispatch goroutine), ForceSwitch (epoll -> io_uring), then
// the standby is polled every 25 ms. The verdict uses the first 1 s only (measured from BEFORE the ForceSwitch
// call); polling continues to 4 s only to report when, if ever, the standby drains.
//
// Measured in step 0 with the post-switch sweep alone and no ask: 7 of 8 runs FAILED, and 0 of 8 with the ask
// (p = 0.0014). It is the test that says P8 is needed and not merely tidy.

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// s0SlowAsyncHandler is an async route that sleeps ~5 ms and writes a tiny 200.
type s0SlowAsyncHandler struct{}

func (s0SlowAsyncHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	time.Sleep(5 * time.Millisecond)
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}},
		[]byte("ok"))
}

func (s0SlowAsyncHandler) RouteAsync(_, _ string) bool { return true }
func (s0SlowAsyncHandler) HasAsyncRoutes() bool        { return true }

var (
	_ stream.Handler            = s0SlowAsyncHandler{}
	_ stream.AsyncRouteResolver = s0SlowAsyncHandler{}
)

func TestSlowAsyncHandlerConverges(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	const (
		conns   = 64
		bound   = time.Second
		horizon = 4 * time.Second
		pollIvl = 25 * time.Millisecond
	)
	e, addr, stop := s0Bind(t, resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, AsyncHandlers: true},
		s0SlowAsyncHandler{})
	defer stop()

	stopLoad := make(chan struct{})
	var okCount, errCount atomic.Int64
	var cen s0Census
	wg := s0Drive(addr, conns, stopLoad, &okCount, &errCount, &cen)

	time.Sleep(500 * time.Millisecond)
	ok500 := okCount.Load()
	time.Sleep(500 * time.Millisecond)
	ok1000 := okCount.Load()
	pm := e.primary.Metrics()
	promoted := pm.AsyncPromotedConns
	// Per-connection request cycle over the last 500 ms of the pre-switch load.
	cycleUs := int64(-1)
	if d := ok1000 - ok500; d > 0 {
		cycleUs = int64(500_000) * conns / d
	}
	t.Logf("S0SA BEFORE epoll=%d promoted=%d wE=%d ok=%d err=%d cycle_us=%d", pm.ActiveConnections, promoted,
		pm.Workers, ok1000, errCount.Load(), cycleUs)

	t0 := time.Now()
	e.ForceSwitch() // epoll -> io_uring: the outgoing (standby) engine is epoll
	took := time.Since(t0)

	convergedMs := int64(-1)
	var at1s, maxWithin int64 = -1, -1
	var trace []string
	nextTrace := time.Duration(0)
	for {
		el := time.Since(t0)
		standby := e.primary.Metrics().ActiveConnections
		if convergedMs < 0 && standby <= 2 {
			convergedMs = el.Milliseconds()
		}
		if el <= bound && standby > maxWithin {
			maxWithin = standby
		}
		if at1s < 0 && el >= bound {
			at1s = standby
		}
		if el >= nextTrace {
			trace = append(trace, fmt.Sprintf("%d:%d", el.Milliseconds(), standby))
			nextTrace += 100 * time.Millisecond
		}
		if el >= horizon || (convergedMs >= 0 && el >= bound) {
			break
		}
		time.Sleep(pollIvl)
	}
	close(stopLoad)
	wg.Wait()
	sm := e.secondary.Metrics()
	t.Logf("S0SA TRACE ms:standby %s", strings.Join(trace, " "))
	t.Logf("S0SA RESULT took_ms=%d converged_ms=%d standby_at_1s=%d wE=%d wI=%d io_uring=%d promoted=%d cycle_us=%d ok=%d err=%d census=%s",
		took.Milliseconds(), convergedMs, at1s, e.primary.Metrics().Workers, sm.Workers, sm.ActiveConnections,
		promoted, cycleUs, okCount.Load(), errCount.Load(), cen.String())

	if promoted == 0 {
		t.Errorf("S0SA PREMISE: no conn was promoted to its dispatch goroutine; the async path was not exercised")
	}
	if convergedMs < 0 || convergedMs > bound.Milliseconds() {
		t.Errorf("S0SA PLACEMENT: the outgoing epoll engine still held %d conns 1 s after the promotion (want <= 2; converged_ms=%d)",
			at1s, convergedMs)
	}
	if n := errCount.Load(); n != 0 {
		t.Errorf("S0SA LOSS: %d of %d keep-alive clients lost a request (census %s)", n, conns, cen.String())
	}
}
