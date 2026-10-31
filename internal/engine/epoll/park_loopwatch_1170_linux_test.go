//go:build linux

package epoll

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#1170: a loop that parks (an adaptive sub-engine demoted to standby, a
// paused engine with no connection) blocks on a Go channel, not in epoll_wait, and
// must not leave the stamp of its last batch behind: the pool handlers' wait reads
// every loop's stamp, and a stale one reads as a loop stuck for ever, which switches
// the rate rule off for the server (CI saw it on adaptive). After a request has been
// served and the loops have parked no loop reads as stuck, however long it parks.
func TestParkedLoopsAreNotStuck1170(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, err := New(resource.Config{
		Addr:               addr,
		Protocol:           engine.HTTP1,
		Resources:          resource.Resources{Workers: 2},
		DisableDeferAccept: true, // no pause linger: the loops park as PauseAccept is called
	}, okHandler658{})
	if err != nil {
		t.Fatalf("epoll engine unavailable: %v", err)
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
	if err := awaitBound(e, done, 8*time.Second); err != nil {
		t.Fatalf("epoll engine did not bind: %v", err)
	}
	// A batch on each loop.
	cl := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}} // no connection stays: the loops can park
	for i := 0; i < 16; i++ {
		resp, err := cl.Get("http://" + addr + "/")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	e.mu.Lock()
	loops := append([]*Loop(nil), e.loops...)
	watch := e.loopWatch
	e.mu.Unlock()
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("pause: %v", err)
	}
	for dl := time.Now().Add(3 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		parked := 0
		for _, l := range loops {
			if l.suspended.Load() {
				parked++
			}
		}
		if parked == len(loops) {
			break
		}
		if time.Now().After(dl) {
			t.Fatalf("PREMISE: the paused loops did not park")
		}
	}
	time.Sleep(300 * time.Millisecond)
	if watch.BlockedFor(time.Now().UnixNano(), int64(100*time.Millisecond)) {
		t.Fatal("a parked loop reads as stuck in a batch: its stamp was left behind")
	}
}
