//go:build linux

package iouring

import (
	"bufio"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/resource"
)

// celeris#884 item 82 (the IOTSTPARK lane's finding, PR #1100): the header-timer
// arm at handleRecv's async feed is LIVE for traffic, and the only test that
// reached it staged the deadline in a unit. In async mode OnHeaderDeadlineArmed
// is nil, so a promoted keep-alive connection whose next request starts after
// the first kernel timer has fired gets its deadline stamped by ProcessH1 on
// the dispatch goroutine, and it is the worker's next feed of that request's
// drip that reads the deadline and arms the kernel timer. Without the arm the
// checkTimeouts sweep still closes the connection at the deadline, so the
// connection's fate alone does not show it: the witness is the worker's own
// state, read from asyncFeedHook on the worker thread. (The arm in
// promoteConnToAsync has no such flow: ProcessH1 clears the deadline before it
// returns ErrAsyncDispatch; see TestInlineAsyncDispatchLeavesNoHeaderDeadline884.)
func TestAsyncFeedArmsTheHeaderTimerOfADrippedRequest884(t *testing.T) {
	const rht = 600 * time.Millisecond
	var mu sync.Mutex
	var armedBefore []bool // cs.headerTimerArmed at each feed, before the feed arms
	var deadlines []int64  // the header deadline each feed read
	hook := func(_ *Worker, cs *connState, hdrDL int64) {
		mu.Lock()
		armedBefore = append(armedBefore, cs.headerTimerArmed)
		deadlines = append(deadlines, hdrDL)
		mu.Unlock()
	}
	asyncFeedHook.Store(&hook)
	t.Cleanup(func() { asyncFeedHook.Store(nil) })

	addr := freeAddr639(t)
	_, cancel, done := startRingRetried662(t, func() (*Engine, error) {
		return New(resource.Config{
			Addr: addr, AsyncHandlers: true, ReadHeaderTimeout: rht,
			Resources: resource.Resources{Workers: 2},
		}, asyncEveryRouteHandler{})
	})
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("engine did not stop within 5s")
		}
	})
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	br := bufio.NewReader(c)
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	get := func() {
		t.Helper()
		if _, err := c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Fatalf("write request: %v", err)
		}
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		_ = resp.Body.Close()
	}
	state := func() (n int, lastArmed bool) {
		mu.Lock()
		defer mu.Unlock()
		if n = len(armedBefore); n > 0 {
			lastArmed = armedBefore[n-1]
		}
		return n, lastArmed
	}

	// The first request promotes the connection. Whole requests follow, each
	// a feed, until one finds no timer in flight: the first one, armed when
	// the protocol was decided, has fired (its CQE found no deadline and did
	// nothing). From then on only a feed can arm one in async mode.
	get()
	for dl := time.Now().Add(rht + 10*time.Second); ; time.Sleep(20 * time.Millisecond) {
		get()
		if n, lastArmed := state(); n > 0 && !lastArmed {
			break
		}
		if time.Now().After(dl) {
			t.Fatalf("apparatus: the first header timer did not fire within %v", rht+10*time.Second)
		}
	}

	// The head of a second request, a header line at a time, with the
	// goroutine given time to stamp the deadline and park between the writes.
	base, _ := state()
	if _, err := c.Write([]byte("GET /drip HTTP/1.1\r\n")); err != nil {
		t.Fatalf("write head: %v", err)
	}
	armedSeen, deadlineSeen := false, false
	for i := 0; i < 12 && !armedSeen; i++ {
		time.Sleep(30 * time.Millisecond)
		if _, err := c.Write([]byte("X-Drip: 1\r\n")); err != nil {
			t.Fatalf("write drip %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		for j := base + 1; j < len(armedBefore); j++ { // base is the head's own feed: nothing is stamped yet
			armedSeen = armedSeen || armedBefore[j]
			deadlineSeen = deadlineSeen || deadlines[j] > 0
		}
		mu.Unlock()
	}
	mu.Lock()
	t.Logf("celeris884.82 feeds=%d (before the drip %d) deadlines=%v armedBefore=%v", len(armedBefore), base, deadlines[base:], armedBefore[base:])
	mu.Unlock()
	if !deadlineSeen {
		t.Fatalf("apparatus: no feed of the drip read a header deadline: the goroutine did not stamp one")
	}
	if !armedSeen {
		t.Errorf("celeris#884 item 82: the feed that read the dripped request's header deadline did not arm the kernel timer (no later feed found headerTimerArmed set)")
	}
}
