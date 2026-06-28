//go:build linux

package adaptive

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/probe"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// respHandler writes a tiny 200 so keep-alive clients can read a real response
// (noopHandler writes nothing, which is fine for churn-close but not for a
// request/response keep-alive load). Sync: runs inline on the worker/loop.
type respHandler struct{}

func (respHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}},
		[]byte("ok"))
}

// asyncRespHandler is respHandler that ALSO marks every route async
// (AsyncRouteResolver), so with Config.AsyncHandlers=true each conn promotes to
// the per-conn dispatch goroutine — exercising the async transplant path.
type asyncRespHandler struct{ respHandler }

func (asyncRespHandler) RouteAsync(_, _ string) bool { return true }
func (asyncRespHandler) HasAsyncRoutes() bool        { return true }

var (
	_ stream.Handler            = respHandler{}
	_ stream.AsyncRouteResolver = asyncRespHandler{}
)

// newBoundAdaptiveH builds an adaptive engine bound to 127.0.0.1:0 with the given
// handler and async flag, starts Listen, waits for bind, and disables the switch
// cooldown so the test can ForceSwitch freely.
func newBoundAdaptiveH(t *testing.T, h stream.Handler, async bool) (*Engine, string, func()) {
	t.Helper()
	if !probe.Probe().IOUringTier.Available() {
		t.Skip("io_uring unavailable: needs both sub-engines")
	}
	e, err := New(resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1, AsyncHandlers: async}, h, nil)
	if err != nil {
		t.Skipf("adaptive.New unsupported here: %v", err)
	}
	e.ctrl.cooldown = 0
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

// driveKeepAlive opens `conns` keep-alive HTTP/1.1 connections that loop
// write-request / read-response until pause is closed, then hold the conn open
// (idle) until stop is closed. Counts oks and errors. The pause phase lets a
// test measure the CONVERGED transplant state (idle conns all migrated) vs the
// in-flight snapshot taken while load is running.
func driveKeepAlive(addr string, conns int, pause, stop <-chan struct{}, ok, errc *atomic.Int64) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
			if derr != nil {
				errc.Add(1)
				return
			}
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			for {
				select {
				case <-stop:
					return
				case <-pause:
					<-stop // hold the conn open + idle until teardown
					return
				default:
				}
				if _, werr := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); werr != nil {
					errc.Add(1)
					return
				}
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				resp, rerr := http.ReadResponse(br, nil)
				if rerr != nil {
					errc.Add(1)
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

// reverseScenario: start on io_uring, run keep-alive load so conns live on
// io_uring, revert io_uring→epoll, and confirm the established conns MIGRATE to
// epoll (not stranded on the now-standby io_uring) while requests keep flowing.
func reverseScenario(t *testing.T, h stream.Handler, async bool) {
	t.Helper()
	e, addr, stop := newBoundAdaptiveH(t, h, async)
	defer stop()

	e.ForceSwitch() // epoll -> io_uring
	time.Sleep(150 * time.Millisecond)

	const conns = 64
	pauseLoad := make(chan struct{})
	stopLoad := make(chan struct{})
	var okCount, errCount atomic.Int64
	wg := driveKeepAlive(addr, conns, pauseLoad, stopLoad, &okCount, &errCount)

	time.Sleep(700 * time.Millisecond)
	epoBefore := e.primary.Metrics().ActiveConnections
	iouBefore := e.secondary.Metrics().ActiveConnections
	promoted := e.secondary.Metrics().AsyncPromotedConns

	e.ForceSwitch() // io_uring -> epoll (fires reverse transplant)
	time.Sleep(1500 * time.Millisecond)
	// In-flight snapshot: taken while load runs, so conns mid-request are still
	// counted on io_uring (async migration is deferred to the goroutine's park).
	epoInflight := e.primary.Metrics().ActiveConnections
	iouInflight := e.secondary.Metrics().ActiveConnections

	// Converged snapshot: pause the load so every conn goes idle → its goroutine
	// parks → self-transplants. This shows the true migration completeness.
	close(pauseLoad)
	time.Sleep(1500 * time.Millisecond)
	epoConverged := e.primary.Metrics().ActiveConnections
	iouConverged := e.secondary.Metrics().ActiveConnections

	close(stopLoad)
	wg.Wait()

	t.Logf("[async=%v] before revert: epoll=%d io_uring=%d (promoted=%d) | in-flight: epoll=%d io_uring=%d | CONVERGED: epoll=%d io_uring=%d | ok=%d err=%d",
		async, epoBefore, iouBefore, promoted, epoInflight, iouInflight, epoConverged, iouConverged, okCount.Load(), errCount.Load())

	if async && promoted == 0 {
		t.Errorf("async test never promoted any conn to the dispatch goroutine — async path not exercised")
	}
	// Converged: essentially all conns must have migrated to epoll.
	if epoConverged < conns-2 {
		t.Errorf("reverse transplant did not converge: epoll=%d (want ~%d)", epoConverged, conns)
	}
	if iouConverged > 2 {
		t.Errorf("io_uring did not drain on convergence: io_uring=%d (want ~0)", iouConverged)
	}
	if errCount.Load() > int64(conns) {
		t.Errorf("too many request errors across the revert: %d (conns=%d)", errCount.Load(), conns)
	}
}

// flapScenario: establish conns on epoll, then flap epoll→io_uring→epoll→io_uring
// under load. Each flap must migrate the same conns to the new active engine
// (forward, reverse, forward) — exercising both transplant directions.
func flapScenario(t *testing.T, h stream.Handler, async bool) {
	t.Helper()
	e, addr, stop := newBoundAdaptiveH(t, h, async)
	defer stop()

	const conns = 64
	noPause := make(chan struct{}) // never closed: flap keeps load running throughout
	stopLoad := make(chan struct{})
	var okCount, errCount atomic.Int64
	wg := driveKeepAlive(addr, conns, noPause, stopLoad, &okCount, &errCount)

	time.Sleep(500 * time.Millisecond) // conns establish on epoll (default active)

	for flap := 1; flap <= 3; flap++ {
		e.ForceSwitch()
		time.Sleep(1200 * time.Millisecond)
		epo := e.primary.Metrics().ActiveConnections
		iou := e.secondary.Metrics().ActiveConnections
		activeIsIOUring := flap%2 == 1
		var active, standby int64
		var activeName string
		if activeIsIOUring {
			active, standby, activeName = iou, epo, "io_uring"
		} else {
			active, standby, activeName = epo, iou, "epoll"
		}
		t.Logf("[async=%v] flap %d -> active=%s: epoll=%d io_uring=%d (ok=%d err=%d)",
			async, flap, activeName, epo, iou, okCount.Load(), errCount.Load())
		if active < conns/2 {
			t.Errorf("flap %d: transplant did not migrate to %s: active=%d (want ~%d)", flap, activeName, active, conns)
		}
		if standby > conns/4 {
			t.Errorf("flap %d: standby did not drain: standby=%d (want ~0)", flap, standby)
		}
	}

	close(stopLoad)
	wg.Wait()
	t.Logf("[async=%v] total ok=%d err=%d | epoll promoted=%d io_uring promoted=%d",
		async, okCount.Load(), errCount.Load(),
		e.primary.Metrics().AsyncPromotedConns, e.secondary.Metrics().AsyncPromotedConns)
	if async && e.primary.Metrics().AsyncPromotedConns == 0 && e.secondary.Metrics().AsyncPromotedConns == 0 {
		t.Errorf("async flap never promoted any conn — async path not exercised")
	}
	if errCount.Load() > int64(conns) {
		t.Errorf("too many request errors across flaps: %d (conns=%d)", errCount.Load(), conns)
	}
}

func TestReverseTransplant(t *testing.T) {
	if testing.Short() {
		t.Skip("reverse-transplant integration test")
	}
	reverseScenario(t, respHandler{}, false)
}

func TestReverseTransplantAsync(t *testing.T) {
	if testing.Short() {
		t.Skip("reverse-transplant async integration test")
	}
	reverseScenario(t, asyncRespHandler{}, true)
}

func TestBidirectionalFlap(t *testing.T) {
	if testing.Short() {
		t.Skip("bidirectional-flap integration test")
	}
	flapScenario(t, respHandler{}, false)
}

func TestBidirectionalFlapAsync(t *testing.T) {
	if testing.Short() {
		t.Skip("bidirectional-flap async integration test")
	}
	flapScenario(t, asyncRespHandler{}, true)
}
