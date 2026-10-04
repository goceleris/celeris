//go:build linux

package adaptive

import (
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// TestSwitchAbortedByADriverDrainsTheFreshStandby (celeris#662 review): a
// switch that builds the lazy io_uring standby and then finds that a driver
// registered during the build aborts, and pauses the fresh engine so it
// leaves the SO_REUSEPORT group. That pause lingers for about 1.5 s, with the
// fresh engine still accepting its share of new connections. What it accepts
// then has to move to the engine that stays active -- the drain a completed
// switch gives the engine it leaves -- and not stay on a standby that no
// switch may ever make active. Every client is served either way; this pins
// where they end up.
func TestSwitchAbortedByADriverDrainsTheFreshStandby(t *testing.T) {
	e, port := startAdaptive662(t, resource.Config{})
	addr := e.Addr().String()
	epollSet := cookies662(listeners662(t, port))

	// A driver registers while the standby is being built, which is what
	// makes performSwitch abort after the build.
	e.mu.Lock()
	build := e.buildStandby
	registered := false
	e.buildStandby = func() (engine.Engine, error) {
		eng, err := build()
		if err == nil {
			e.acquireDriverFD()
			registered = true
		}
		return eng, err
	}
	e.mu.Unlock()
	rejected0 := e.SwitchRejectedCount()
	e.ForceSwitch()
	if registered {
		e.releaseDriverFD()
	}
	e.mu.Lock()
	iou, ep := e.secondary, e.primary
	e.mu.Unlock()
	if !registered || iou == nil {
		skipOrFailUpswitch662(t, "the io_uring standby was not built here")
	}
	if got := e.ActiveEngine().Type(); got != engine.Epoll {
		t.Fatalf("the aborted switch left %v active, want epoll", got)
	}
	if got := e.SwitchRejectedCount() - rejected0; got != 1 {
		t.Fatalf("premise: SwitchRejectedCount moved by %d, want 1: the switch did not abort", got)
	}
	iouringSet := cookies662(without662(listeners662(t, port), epollSet))
	if len(iouringSet) == 0 {
		t.Fatal("premise: the fresh io_uring standby has no listener right after the aborted " +
			"switch, so it is not lingering and nothing can land on it")
	}

	// Keep-alive clients, one request each, while the fresh engine lingers.
	const n = 32
	var conns []net.Conn
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})
	for range n {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		conns = append(conns, c)
		if _, err := c.Write([]byte(idleSwitchReq662)); err != nil {
			t.Fatalf("write: %v", err)
		}
		if o := readOutcome662(c); o != "200" {
			t.Fatalf("a client's first request ended %s", o)
		}
	}
	accepted := iou.Metrics().AcceptCount
	if accepted == 0 {
		t.Fatalf("premise: none of the %d clients landed on the lingering io_uring standby", n)
	}

	closedAfter := waitGone662(t, port, iouringSet, "the aborted io_uring standby")
	var left int64
	for dl := time.Now().Add(5 * time.Second); ; {
		left = iou.Metrics().ActiveConnections
		if left == 0 || time.Now().After(dl) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	served := 0
	for _, c := range conns {
		if _, err := c.Write([]byte(idleSwitchReq662)); err == nil && readOutcome662(c) == "200" {
			served++
		}
	}
	im, em := iou.Metrics(), ep.Metrics()
	t.Logf("io_uring standby: accepted %d of %d during its linger, listeners closed %v after the "+
		"clients, still holding %d; transplant detached=%d | epoll adopted=%d active=%d | served "+
		"%d of %d after", accepted, n, closedAfter, left, im.TransplantDetached, em.TransplantAdopted,
		em.ActiveConnections, served, n)
	if left != 0 {
		t.Errorf("%d of the %d connections the aborted io_uring standby accepted during its linger "+
			"are still on it: nothing moves them to the engine that stayed active", left, accepted)
	}
	if served != n {
		t.Errorf("%d of %d clients were served on their second request", served, n)
	}
}
