//go:build linux

package iouring

// celeris#713. A worker stamps lastActivity from w.cachedNow, a clock it
// refreshes only on its own iterations (every 64th CQE-bearing one, and in
// checkTimeouts). The DRAINING→SUSPENDED park stops those iterations, so a
// worker leaves the park with the clock it parked with. The connections it
// accepts or adopts on waking were stamped from that clock, and the first
// checkTimeouts after the wake, which compares against a fresh time.Now(),
// saw them idle for the whole park: past ReadTimeout, it closed them, with a
// request already written. Measured through the adaptive engine as 21-156
// closes at 6 of 6 promotes that came 31 s after a demote (ReadTimeout 30 s),
// and none at 18 s.
//
// Here that is one engine and no adaptive controller: park the workers for
// longer than ReadTimeout, wake one with a new connection (an accept after
// ResumeAccept, or an AdoptConn), and keep that connection busy well inside
// ReadTimeout. It must be served throughout. The short-park arms are the
// controls: the same wake and the same traffic after a park well inside
// ReadTimeout.

import (
	"bufio"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/resource"
)

const (
	staleClockReadTimeout = 2 * time.Second
	staleClockRequests    = 20
	staleClockGap         = 100 * time.Millisecond
)

// staleClockAfterPark parks every worker for park, wakes one with a new
// connection (adopt: AdoptConn on the still-paused engine; otherwise
// ResumeAccept and a dial), and serves staleClockRequests requests on it,
// staleClockGap apart: two seconds of traffic that no timeout may interrupt.
func staleClockAfterPark(t *testing.T, park time.Duration, adopt bool) {
	e, addr := startFDLEngine(t, fdlHandler{}, func(c *resource.Config) {
		// No TCP_DEFER_ACCEPT: no pause linger (celeris#662), so the
		// listeners close as PauseAccept is called and the workers park at
		// once, and startFDLEngine's silent probe dial is accepted at once.
		c.DisableDeferAccept = true
		c.ReadTimeout = staleClockReadTimeout
		c.IdleTimeout = 10 * time.Minute
	})
	e.mu.Lock()
	ws := append([]*Worker(nil), e.workers...)
	e.mu.Unlock()
	allParked := func() bool {
		for _, w := range ws {
			if !w.suspended.Load() {
				return false
			}
		}
		return true
	}
	waitFor := func(d time.Duration, f func() bool) bool {
		for dl := time.Now().Add(d); ; {
			if f() {
				return true
			}
			if time.Now().After(dl) {
				return false
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	if !waitFor(3*time.Second, func() bool {
		m := e.Metrics()
		return m.ActiveConnections == 0 && m.AcceptCount >= 1 && m.AcceptCount == m.CloseCount
	}) {
		t.Fatalf("celeris713 PREMISE: startFDLEngine's probe connection is still live")
	}
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if !waitFor(3*time.Second, allParked) {
		t.Fatalf("celeris713 PREMISE: the paused workers did not park")
	}
	parkedAt := time.Now()
	time.Sleep(park)
	if !allParked() {
		t.Fatalf("celeris713 PREMISE: a worker left the park on its own")
	}

	m0 := e.Metrics()
	var c net.Conn
	if adopt {
		client, fd := adoptPair658(t)
		if err := e.AdoptConn(fd, engine.Carryover{RemoteAddr: client.LocalAddr().String()}); err != nil {
			_ = unix.Close(fd)
			t.Fatalf("AdoptConn: %v", err)
		}
		if !waitFor(2*time.Second, func() bool { return e.Metrics().TransplantAdopted == m0.TransplantAdopted+1 }) {
			t.Fatalf("celeris713 PREMISE: the adoption did not complete")
		}
		c = client
	} else {
		if err := e.ResumeAccept(); err != nil {
			t.Fatalf("resume: %v", err)
		}
		var err error
		for dl := time.Now().Add(3 * time.Second); ; {
			if c, err = net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
				break
			}
			if time.Now().After(dl) {
				t.Fatalf("celeris713 PREMISE: no listener after ResumeAccept: %v", err)
			}
			time.Sleep(5 * time.Millisecond)
		}
		defer func() { _ = c.Close() }()
	}
	woke := time.Since(parkedAt)

	br := bufio.NewReader(c)
	served, failed, why := 0, -1, ""
	for i := range staleClockRequests {
		if i > 0 {
			time.Sleep(staleClockGap)
		}
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			failed, why = i, "write: "+err.Error()
			break
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			failed, why = i, "read: "+err.Error()
			break
		}
		_ = resp.Body.Close()
		served++
	}
	m := e.Metrics()
	t.Logf("celeris713 adopt=%v park=%v woke_after=%v read_timeout=%v workers=%d served=%d/%d failed_at=%d (%s) closes=%d",
		adopt, park, woke.Round(time.Millisecond), staleClockReadTimeout, len(ws), served, staleClockRequests,
		failed, why, m.CloseCount-m0.CloseCount)
	if failed >= 0 {
		t.Errorf("celeris713 STALE: request %d of a connection %s %v after the park began, %v apart, failed "+
			"(%s): the worker closed it on ReadTimeout %v, measured from the clock it parked with",
			failed, map[bool]string{true: "adopted", false: "accepted"}[adopt], woke.Round(time.Millisecond),
			staleClockGap, why, staleClockReadTimeout)
	}
}

// TestAcceptAfterALongParkIsNotTimedOut: a ResumeAccept after a park longer
// than ReadTimeout, then a new connection kept busy. The defect.
func TestAcceptAfterALongParkIsNotTimedOut(t *testing.T) {
	staleClockAfterPark(t, staleClockReadTimeout+time.Second, false)
}

// TestAdoptAfterALongParkIsNotTimedOut: an AdoptConn that wakes a worker
// parked for longer than ReadTimeout. The adaptive promote's shape.
func TestAdoptAfterALongParkIsNotTimedOut(t *testing.T) {
	staleClockAfterPark(t, staleClockReadTimeout+time.Second, true)
}

// TestAcceptAfterAShortParkIsNotTimedOut is the control: the same wake and
// the same traffic after a park well inside ReadTimeout.
func TestAcceptAfterAShortParkIsNotTimedOut(t *testing.T) {
	staleClockAfterPark(t, 100*time.Millisecond, false)
}

// TestAdoptAfterAShortParkIsNotTimedOut is the adoption's control.
func TestAdoptAfterAShortParkIsNotTimedOut(t *testing.T) {
	staleClockAfterPark(t, 100*time.Millisecond, true)
}
