//go:build linux

package adaptive

// celeris#657 P11 (B0): the engine a switch makes ACTIVE must not still be
// draining into the engine that switch is making standby.
//
// applyTransplant rewires both drains, but it runs at the END of
// performSwitch. Between the new active resuming its listener and that
// rewiring, the new active still carries the drain it was given as a SOURCE at
// the previous switch — pointing at the engine now being switched away from.
// Every connection it accepts in that window is at a clean HTTP/1 boundary the
// moment it has answered its first request, so it is handed straight back to
// the outgoing engine. Measured as a ping-pong of 80 connections in 1 of 150
// switches; with the post-switch sweep on, the connection does not even need
// to send a request.
//
// The window is normally a few milliseconds, which is why the effect was rare.
// switchWindowHook makes it as long as the test needs, so the observation is
// deterministic rather than a race the test might lose.

import (
	"bufio"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

func TestSwitchDoesNotAdoptOntoOutgoing(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	e, addr, stop := s0Bind(t, resource.Config{Addr: "127.0.0.1:0", Protocol: engine.HTTP1}, respHandler{})
	defer stop()

	// Switch 1, with nothing connected: epoll becomes standby and is given
	// the drain toward io_uring. That drain is what switch 2 must stop
	// before epoll starts accepting again.
	e.ForceSwitch()
	time.Sleep(500 * time.Millisecond)
	if n := e.secondary.Metrics().TransplantAdopted; n != 0 {
		t.Fatalf("celeris657 B0 PREMISE: io_uring adopted %d conns before the test opened any", n)
	}

	var (
		once      sync.Once
		fired     bool
		dialErr   error
		onEpoll   int64
		onIOUring int64
		adoptedIn int64
	)
	switchWindowHook = func(newActive, newStandby engine.Engine) {
		if newActive != e.primary {
			return // switch 1: the window this test is about is switch 2's
		}
		once.Do(func() {
			fired = true
			// The new active (epoll) is listening and the outgoing engine
			// (io_uring) is paused, so this connection can only be accepted
			// by the new active.
			var c net.Conn
			for dl := time.Now().Add(3 * time.Second); time.Now().Before(dl); {
				c, dialErr = net.DialTimeout("tcp", addr, 500*time.Millisecond)
				if dialErr == nil {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if dialErr != nil {
				return
			}
			defer func() { _ = c.Close() }()
			if _, dialErr = c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); dialErr != nil {
				return
			}
			_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
			var resp *http.Response
			if resp, dialErr = http.ReadResponse(bufio.NewReader(c), nil); dialErr != nil {
				return
			}
			_ = resp.Body.Close()
			// Give the stale drain the time it needs to act. A hand-off
			// the base makes lands well inside this.
			for dl := time.Now().Add(700 * time.Millisecond); time.Now().Before(dl); {
				if newStandby.Metrics().TransplantAdopted > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			onEpoll = e.primary.Metrics().ActiveConnections
			onIOUring = e.secondary.Metrics().ActiveConnections
			adoptedIn = int64(newStandby.Metrics().TransplantAdopted)
		})
	}
	defer func() { switchWindowHook = nil }()

	e.ForceSwitch() // switch 2: io_uring -> epoll
	time.Sleep(300 * time.Millisecond)

	adoptedAfter := int64(e.secondary.Metrics().TransplantAdopted)
	t.Logf("celeris657 B0 fired=%v dial_err=%v in_window: epoll=%d io_uring=%d iouring_adopted=%d | "+
		"after: iouring_adopted=%d epoll=%d io_uring=%d",
		fired, dialErr, onEpoll, onIOUring, adoptedIn, adoptedAfter,
		e.primary.Metrics().ActiveConnections, e.secondary.Metrics().ActiveConnections)

	if !fired {
		t.Fatal("celeris657 B0 PREMISE: the switch-window hook never ran for the switch back to epoll")
	}
	if dialErr != nil {
		t.Fatalf("celeris657 B0 PREMISE: the in-window client failed: %v", dialErr)
	}
	if onEpoll+onIOUring == 0 {
		t.Fatal("celeris657 B0 PREMISE: the in-window connection was on neither engine")
	}
	if adoptedIn != 0 {
		t.Errorf("celeris657 B0: the engine this switch is leaving adopted %d conns DURING the switch — the new "+
			"active accepted them while still draining into it (it was on epoll=%d io_uring=%d)",
			adoptedIn, onEpoll, onIOUring)
	}
	if adoptedAfter != 0 {
		t.Errorf("celeris657 B0: io_uring adopted %d conns across a switch that made it the standby; every "+
			"connection accepted after the switch belongs to the new active", adoptedAfter)
	}
}
