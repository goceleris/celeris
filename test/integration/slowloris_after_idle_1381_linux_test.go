//go:build linux

package integration

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/epoll"
	"github.com/goceleris/celeris/internal/engine/iouring"
	"github.com/goceleris/celeris/internal/resource"
)

// TestSlowlorisAfterAnIdleEngine1381 is TestSlowlorisSynthetic's scenario on an
// engine that has been idle for longer than its loops need to reach their
// longest wait (celeris#1381: a loop that holds nothing waits 100 ms and ticks
// its sweep timer at that period). The first connection after the idle must
// have the header deadline enforced as on a fresh engine: the client sends a
// partial request and nothing more, and the engine must close it at
// ReadHeaderTimeout, within the same 2 s slack the synthetic test allows.
func TestSlowlorisAfterAnIdleEngine1381(t *testing.T) {
	const (
		idle    = 1500 * time.Millisecond // past 100 empty rounds on both engines
		rht     = 2 * time.Second
		maxLate = 2 * time.Second
	)
	for _, tc := range []struct {
		name   string
		newEng func(cfg resource.Config) (engine.Engine, error)
	}{
		{"iouring", func(cfg resource.Config) (engine.Engine, error) { return iouring.New(cfg, &echoHandler{}) }},
		{"epoll", func(cfg resource.Config) (engine.Engine, error) { return epoll.New(cfg, &echoHandler{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := resource.Config{
				Addr:              "127.0.0.1:0",
				Protocol:          engine.HTTP1,
				ReadHeaderTimeout: rht,
				ReadTimeout:       30 * time.Second,
				WriteTimeout:      30 * time.Second,
				IdleTimeout:       120 * time.Second,
			}
			eng := startEngineRetry(t, func() (engine.Engine, error) { return tc.newEng(cfg) },
				func(err error) { engineNewFailed890(t, tc.name, err) })
			ag, ok := eng.(addrGetter)
			if !ok {
				t.Fatalf("%s engine has no Addr()", tc.name)
			}
			time.Sleep(idle) // the engine is idle: nothing is connected, nothing is queued

			conn, err := net.DialTimeout("tcp", ag.Addr().String(), 2*time.Second)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: test\r\nX-Slow: ")); err != nil {
				t.Fatalf("partial request write: %v", err)
			}
			t0 := time.Now()
			_ = conn.SetReadDeadline(t0.Add(rht + 4*time.Second))
			// The engine may answer (a 408) before it closes: read to the end.
			buf := make([]byte, 512)
			for {
				_, err := conn.Read(buf)
				if err == nil {
					continue
				}
				if !errors.Is(err, io.EOF) {
					var ne net.Error
					if errors.As(err, &ne) && ne.Timeout() {
						t.Fatalf("[%s] the connection was not closed %v after a partial request on an engine that had been idle for %v: the header deadline did not fire",
							tc.name, time.Since(t0), idle)
					}
					// A reset is a close too.
				}
				break
			}
			late := time.Since(t0) - rht
			t.Logf("celeris1381 SLOWLORIS-AFTER-IDLE [%s]: closed %v after the partial request (ReadHeaderTimeout %v, slack allowed %v)", tc.name, time.Since(t0).Round(time.Millisecond), rht, maxLate)
			if late > maxLate {
				t.Errorf("[%s] closed %v after ReadHeaderTimeout, want within %v", tc.name, late, maxLate)
			}
			if late < -300*time.Millisecond {
				t.Errorf("[%s] closed %v BEFORE ReadHeaderTimeout: a deadline that fired early", tc.name, -late)
			}
		})
	}
}
