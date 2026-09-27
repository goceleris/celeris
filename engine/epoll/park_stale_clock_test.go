//go:build linux

package epoll

// celeris#713, the epoll twin. The io_uring worker left its DRAINING→SUSPENDED
// park with the clock it parked with, and stamped the connections it accepted
// or adopted on waking from it; the first checkTimeouts then closed them on
// ReadTimeout. An epoll loop parks the same way, on a Go channel, and it
// stamps from l.cachedNow too, but it refreshes that clock on every
// events-bearing epoll_wait return, and a wake by ResumeAccept or AdoptConn
// comes back with an event (the new listener's accept, the adopt queue's
// eventfd). These are the io_uring tests' arms on this engine: they pin that
// behaviour, so a change to the refresh cannot bring the defect here.

import (
	"bufio"
	"context"
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

func staleClockAfterParkEpoll(t *testing.T, park time.Duration, adopt bool) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr:      addr,
		Protocol:  engine.HTTP1,
		Resources: resource.Resources{Workers: 2},
		// No TCP_DEFER_ACCEPT: no pause linger (celeris#662), so the
		// listeners close as PauseAccept is called and the loops park.
		DisableDeferAccept: true,
		ReadTimeout:        staleClockReadTimeout,
		WriteTimeout:       staleClockReadTimeout, // as the io_uring arms: see there
		IdleTimeout:        10 * time.Minute,
	}, okHandler658{})
	if err != nil {
		t.Skipf("epoll engine unavailable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("engine did not stop within 5s")
		}
	})
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
	if !waitFor(8*time.Second, func() bool { return e.Addr() != nil }) {
		t.Skip("epoll engine did not bind")
	}
	e.mu.Lock()
	loops := append([]*Loop(nil), e.loops...)
	e.mu.Unlock()
	allParked := func() bool {
		for _, l := range loops {
			if !l.suspended.Load() {
				return false
			}
		}
		return true
	}
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if !waitFor(3*time.Second, allParked) {
		t.Fatalf("celeris713 PREMISE: the paused loops did not park")
	}
	parkedAt := time.Now()
	time.Sleep(park)
	if !allParked() {
		t.Fatalf("celeris713 PREMISE: a loop left the park on its own")
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
	t.Logf("celeris713 epoll adopt=%v park=%v woke_after=%v read_timeout=%v loops=%d served=%d/%d failed_at=%d (%s) closes=%d",
		adopt, park, woke.Round(time.Millisecond), staleClockReadTimeout, len(loops), served, staleClockRequests,
		failed, why, m.CloseCount-m0.CloseCount)
	if failed >= 0 {
		t.Errorf("celeris713 STALE: request %d of a connection %s %v after the park began, %v apart, failed "+
			"(%s): the loop closed it on ReadTimeout %v, measured from the clock it parked with",
			failed, map[bool]string{true: "adopted", false: "accepted"}[adopt], woke.Round(time.Millisecond),
			staleClockGap, why, staleClockReadTimeout)
	}
}

func TestAcceptAfterALongParkIsNotTimedOutEpoll(t *testing.T) {
	staleClockAfterParkEpoll(t, staleClockReadTimeout+time.Second, false)
}

func TestAdoptAfterALongParkIsNotTimedOutEpoll(t *testing.T) {
	staleClockAfterParkEpoll(t, staleClockReadTimeout+time.Second, true)
}
