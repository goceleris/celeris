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
// request/response keep-alive load).
type respHandler struct{}

func (respHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	return s.ResponseWriter.WriteResponse(s, 200,
		[][2]string{{"content-type", "text/plain"}, {"content-length", "2"}},
		[]byte("ok"))
}

// TestReverseTransplant exercises the #383 REVERSE direction (io_uring→epoll):
// start on io_uring, run a steady keep-alive load so conns live on io_uring,
// then revert io_uring→epoll and confirm the established conns MIGRATE to epoll
// (not stranded on the now-standby io_uring) while requests keep succeeding.
func TestReverseTransplant(t *testing.T) {
	if testing.Short() {
		t.Skip("reverse-transplant integration test")
	}
	if !probe.Probe().IOUringTier.Available() {
		t.Skip("io_uring unavailable: needs both sub-engines")
	}
	e, err := New(resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1}, respHandler{}, nil)
	if err != nil {
		t.Skipf("adaptive.New unsupported here: %v", err)
	}
	e.ctrl.cooldown = 0
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	defer func() { cancel(); <-done }()
	for dl := time.Now().Add(3 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("adaptive engine never bound")
	}
	addr := e.Addr().String()

	// Start on io_uring so the conns land + live there.
	e.ForceSwitch() // epoll -> io_uring
	time.Sleep(150 * time.Millisecond)

	const conns = 64
	var wg sync.WaitGroup
	stopLoad := make(chan struct{})
	var okCount, errCount atomic.Int64
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
			if derr != nil {
				errCount.Add(1)
				return
			}
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			for {
				select {
				case <-stopLoad:
					return
				default:
				}
				if _, werr := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); werr != nil {
					errCount.Add(1)
					return
				}
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				resp, rerr := http.ReadResponse(br, nil)
				if rerr != nil {
					errCount.Add(1)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				okCount.Add(1)
			}
		}()
	}

	// Let the conns establish + run on io_uring.
	time.Sleep(700 * time.Millisecond)
	epoBefore := e.primary.Metrics().ActiveConnections   // epoll
	iouBefore := e.secondary.Metrics().ActiveConnections // io_uring

	// Revert io_uring -> epoll: this fires the reverse transplant.
	e.ForceSwitch()

	// Drain window: conns migrate to epoll as their requests hit a boundary.
	time.Sleep(1500 * time.Millisecond)
	epoAfter := e.primary.Metrics().ActiveConnections
	iouAfter := e.secondary.Metrics().ActiveConnections

	close(stopLoad)
	wg.Wait()

	t.Logf("before revert: epoll=%d io_uring=%d | after revert: epoll=%d io_uring=%d | ok=%d err=%d",
		epoBefore, iouBefore, epoAfter, iouAfter, okCount.Load(), errCount.Load())

	if iouBefore < conns/2 {
		t.Logf("WARN: only %d/%d conns were on io_uring before revert", iouBefore, conns)
	}
	if epoAfter < conns/2 {
		t.Errorf("reverse transplant did not migrate conns to epoll: epoll=%d after revert (want ~%d)", epoAfter, conns)
	}
	if iouAfter > conns/4 {
		t.Errorf("io_uring did not drain on revert: io_uring=%d after revert (want ~0)", iouAfter)
	}
	// Some transient errors during the switch window are acceptable; a gross
	// failure (every conn erroring) is not.
	if errCount.Load() > int64(conns) {
		t.Errorf("too many request errors across the revert: %d (conns=%d)", errCount.Load(), conns)
	}
}

// TestBidirectionalFlap establishes keep-alive conns on epoll, then flaps the
// engine epoll→io_uring→epoll→io_uring under sustained load. Each flap must
// migrate the SAME established conns to the newly active engine (forward then
// reverse then forward), proving both transplant directions work back-to-back
// and that StartTransplant/StopTransplant don't strand conns mid-flap.
func TestBidirectionalFlap(t *testing.T) {
	if testing.Short() {
		t.Skip("bidirectional-flap integration test")
	}
	if !probe.Probe().IOUringTier.Available() {
		t.Skip("io_uring unavailable: needs both sub-engines")
	}
	e, err := New(resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1}, respHandler{}, nil)
	if err != nil {
		t.Skipf("adaptive.New unsupported here: %v", err)
	}
	e.ctrl.cooldown = 0
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	defer func() { cancel(); <-done }()
	for dl := time.Now().Add(3 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
		time.Sleep(10 * time.Millisecond)
	}
	if e.Addr() == nil {
		t.Fatal("adaptive engine never bound")
	}
	addr := e.Addr().String()

	const conns = 64
	var wg sync.WaitGroup
	stopLoad := make(chan struct{})
	var okCount, errCount atomic.Int64
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
			if derr != nil {
				errCount.Add(1)
				return
			}
			defer func() { _ = c.Close() }()
			br := bufio.NewReader(c)
			for {
				select {
				case <-stopLoad:
					return
				default:
				}
				if _, werr := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); werr != nil {
					errCount.Add(1)
					return
				}
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				resp, rerr := http.ReadResponse(br, nil)
				if rerr != nil {
					errCount.Add(1)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				okCount.Add(1)
			}
		}()
	}

	// Conns establish on epoll (the default active engine).
	time.Sleep(500 * time.Millisecond)

	// Flap: each ForceSwitch toggles active epoll<->io_uring. After settling,
	// the active engine must hold ~all conns and the standby ~none.
	for flap := 1; flap <= 3; flap++ {
		e.ForceSwitch()
		time.Sleep(1200 * time.Millisecond)
		epo := e.primary.Metrics().ActiveConnections   // epoll
		iou := e.secondary.Metrics().ActiveConnections // io_uring
		// After odd flaps active=io_uring; after even flaps active=epoll.
		activeIsIOUring := flap%2 == 1
		var active, standby int64
		var activeName string
		if activeIsIOUring {
			active, standby, activeName = iou, epo, "io_uring"
		} else {
			active, standby, activeName = epo, iou, "epoll"
		}
		t.Logf("flap %d -> active=%s: epoll=%d io_uring=%d (ok=%d err=%d)",
			flap, activeName, epo, iou, okCount.Load(), errCount.Load())
		if active < conns/2 {
			t.Errorf("flap %d: transplant did not migrate to %s: active=%d (want ~%d)", flap, activeName, active, conns)
		}
		if standby > conns/4 {
			t.Errorf("flap %d: standby did not drain: standby=%d (want ~0)", flap, standby)
		}
	}

	close(stopLoad)
	wg.Wait()
	t.Logf("total ok=%d err=%d", okCount.Load(), errCount.Load())
	if errCount.Load() > int64(conns) {
		t.Errorf("too many request errors across flaps: %d (conns=%d)", errCount.Load(), conns)
	}
}
