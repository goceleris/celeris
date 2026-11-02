//go:build linux

package adaptive

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
	"github.com/goceleris/celeris/internal/sockopts/sockoptstest"
)

// TestBusyPollSurvivesAnAdaptiveSwitch is celeris#1036 across a switch, in
// both directions. TestAcceptedSocketBusyPollIsOptIn reads sockets the engine
// adaptive STARTED on accepted; the standby is built later (adaptive.New's
// standbyCfg, at the first switch), and a connection that crosses is given
// its socket options again by the engine that takes it (iouring/transplant.go,
// epoll/adopt.go). Each of those is a place the old 50 us could survive or the
// configured value could be dropped, so after ForceSwitch this reads
// SO_BUSY_POLL on connections the promoted standby accepted AND on one opened
// before the switch, which the other engine now owns.
func TestBusyPollSurvivesAnAdaptiveSwitch(t *testing.T) {
	base := sockoptstest.Baseline(t)
	for _, start := range []struct {
		env      string
		from, to engine.EngineType
	}{
		{"epoll", engine.Epoll, engine.IOUring},
		{"iouring", engine.IOUring, engine.Epoll},
	} {
		for _, tc := range []struct {
			name string
			d    time.Duration
			want int
		}{
			{"default", 0, base},
			{"37us", 37 * time.Microsecond, 37},
		} {
			t.Run(start.env+"/"+tc.name, func(t *testing.T) {
				t.Setenv("CELERIS_ADAPTIVE_START", start.env)
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("pick port: %v", err)
				}
				addr := ln.Addr().String()
				_ = ln.Close()
				e, err := New(resource.Config{
					Addr:      addr,
					Engine:    engine.Adaptive,
					Protocol:  engine.HTTP1,
					Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
					Resources: resource.Resources{Workers: 2, SocketBusyPoll: tc.d},
				}, respHandler{}, nil)
				if err != nil {
					t.Fatalf("adaptive.New: %v", err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				ended := make(chan struct{})
				go func() { done <- e.Listen(ctx); close(ended) }()
				t.Cleanup(func() {
					cancel()
					select {
					case <-ended:
					case <-time.After(10 * time.Second):
						t.Error("adaptive engine did not stop within 10s")
					}
				})
				awaitBoundOrFail890(t, e, cancel, done, ended)
				if a := e.ActiveEngine().Type(); a != start.from {
					t.Fatalf("PREMISE: started on %v, want %v", a, start.from)
				}

				// A connection that is open, and has been served, when the switch happens.
				pre, err := net.DialTimeout("tcp", addr, 3*time.Second)
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				t.Cleanup(func() { _ = pre.Close() })
				sockoptstest.RoundTrip(t, pre)
				if got := sockoptstest.AcceptedBusyPoll(t, pre); got != tc.want {
					t.Errorf("before the switch, on %v: SO_BUSY_POLL = %d, want %d", start.from, got, tc.want)
				}

				e.ForceSwitch()
				deadline := time.Now().Add(10 * time.Second)
				for e.ActiveEngine().Type() != start.to && time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
				}
				if a := e.ActiveEngine().Type(); a != start.to {
					t.Fatalf("PREMISE: no switch to %v (active %v)", start.to, a)
				}
				time.Sleep(300 * time.Millisecond) // the listener hand-over settles

				for i := range 4 {
					c, err := net.DialTimeout("tcp", addr, 3*time.Second)
					if err != nil {
						t.Fatalf("dial after the switch: %v", err)
					}
					t.Cleanup(func() { _ = c.Close() })
					sockoptstest.RoundTrip(t, c)
					if got := sockoptstest.AcceptedBusyPoll(t, c); got != tc.want {
						t.Errorf("connection %d accepted by the standby, now on %v: SO_BUSY_POLL = %d, want %d (baseline %d)",
							i, start.to, got, tc.want, base)
					}
				}
				sockoptstest.RoundTrip(t, pre)
				if got := sockoptstest.AcceptedBusyPoll(t, pre); got != tc.want {
					t.Errorf("the connection opened before the switch, moved to %v: SO_BUSY_POLL = %d, want %d (baseline %d)",
						start.to, got, tc.want, base)
				}
			})
		}
	}
}
