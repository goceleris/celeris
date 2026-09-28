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
// ReadTimeout. It must be served throughout. The short-park accept arm is the
// control: the same wake and the same traffic after a park well inside
// ReadTimeout (the short-park adoption arm is not one; see there).

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// startParkEngine713 is startFDLEngine with its ring ENOMEM retried
// (startRingRetried662): at CI's 8 MiB memlock the kernel gives a closed
// ring's pages back 12-23 ms after the close, so an engine started right
// after another ring closed can fail on memory nothing holds any more. No
// probe dial: the engine is idle when it returns.
func startParkEngine713(t *testing.T, h stream.Handler, mut func(*resource.Config)) (*Engine, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, cancel, done := startRingRetried662(t, func() (*Engine, error) {
		cfg := resource.Config{
			Addr:      addr,
			Protocol:  engine.HTTP1,
			Resources: resource.Resources{Workers: 2},
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		if mut != nil {
			mut(&cfg)
		}
		return New(cfg, h)
	})
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("engine did not stop within 5s")
		}
	})
	t.Logf("celeris657 engine workers=%d", e.NumWorkers())
	return e, addr
}

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
	e, addr := startParkEngine713(t, fdlHandler{}, func(c *resource.Config) {
		// No TCP_DEFER_ACCEPT: no pause linger (celeris#662), so the
		// listeners close as PauseAccept is called and the workers park at
		// once.
		c.DisableDeferAccept = true
		c.ReadTimeout = staleClockReadTimeout
		// checkTimeouts judges a connection with a SEND in flight against
		// WriteTimeout instead (60 s by default), so with the default a
		// stale stamp was forgiven whenever the first check after the wake
		// caught the response on its way out: on a paused engine, whose
		// worker iterates only on this connection's own completions, about
		// half the time. The same bound on both keeps the arm from depending
		// on where in a request that check lands.
		c.WriteTimeout = staleClockReadTimeout
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
		return m.ActiveConnections == 0 && m.AcceptCount == m.CloseCount
	}) {
		t.Fatalf("celeris713 PREMISE: the engine is not idle")
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

// TestAdoptAfterAShortParkIsNotTimedOut is the adoption arm after a 100 ms
// park. It is NOT a control: on main it failed too, 10 of 10 in both
// shapes. A paused worker's clock stays at its last refresh before the park
// until its first checkTimeouts after the wake, and on a paused engine the
// worker iterates only on this connection's own completions, so that check
// comes 32 iterations, about 13 requests, into the traffic. The idle time
// before the park, the park and that stretch together pass ReadTimeout. The
// accept arm above is the control: a resumed worker's short listener waits
// bring its first check within a request or two.
func TestAdoptAfterAShortParkIsNotTimedOut(t *testing.T) {
	staleClockAfterPark(t, 100*time.Millisecond, true)
}

// TestAdoptionIsStampedWithTheTimeItWasAdopted is the same rule without the
// park, on a worker whose clock is stale for any other reason: cachedNow is
// refreshed only every 64th CQE-bearing iteration and by checkTimeouts, so a
// draining worker waiting out 1 s ring waits can hold a clock tens of seconds
// old. Injected here directly: an hour. The adopted connection's lastActivity
// must be the time of the adoption, or checkTimeouts reads the hour as idle
// time.
func TestAdoptionIsStampedWithTheTimeItWasAdopted(t *testing.T) {
	f := newFDLFixture(t, false)
	w := f.w
	fd, peer := socketPairFDs(t)
	t.Cleanup(func() { _ = unix.Close(peer) })
	_ = unix.SetNonblock(fd, true)
	if fd >= len(w.conns) {
		t.Fatalf("celeris713 PREMISE: fd %d is past the fixture's table (%d)", fd, len(w.conns))
	}
	t0 := time.Now().UnixNano()
	w.cachedNow = t0 - int64(time.Hour)
	w.attachAdoptedFD(fd, engine.Carryover{RemoteAddr: "127.0.0.1:1"})
	cs := w.conns[fd]
	if cs == nil {
		t.Fatalf("celeris713 PREMISE: the adoption was refused")
	}
	age := time.Duration(t0 - cs.lastActivity)
	t.Logf("celeris713 adopt stamp: lastActivity is %v before the adoption began (worker clock %v stale)",
		age, time.Duration(t0-w.cachedNow))
	if cs.lastActivity < t0 {
		t.Errorf("celeris713 STALE: an adopted connection's lastActivity is %v before its adoption: it was "+
			"stamped from the worker's cached clock, and checkTimeouts reads that as idle time", age)
	}
}
