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

// TestAcceptedSocketBusyPollIsOptIn is celeris#1036 on the
// adaptive engine, on each of the two sub-engines it can start on: the
// accepted socket carries no SO_BUSY_POLL unless Resources.SocketBusyPoll
// asks for it, and then exactly the budget asked for. Both sub-engines are
// built from the one Config adaptive.New normalises, so a field one of them
// does not read is the defect this catches.
func TestAcceptedSocketBusyPollIsOptIn(t *testing.T) {
	base := sockoptstest.Baseline(t)
	for _, start := range []struct {
		env  string
		want engine.EngineType
	}{
		{"epoll", engine.Epoll},
		{"iouring", engine.IOUring},
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
				if active := e.ActiveEngine().Type(); active != start.want {
					t.Fatalf("PREMISE: the adaptive engine started on %v, want %v: this subtest would not cover that sub-engine", active, start.want)
				}
				for range 4 {
					c, err := net.DialTimeout("tcp", addr, 3*time.Second)
					if err != nil {
						t.Fatalf("dial: %v", err)
					}
					t.Cleanup(func() { _ = c.Close() })
					sockoptstest.RoundTrip(t, c)
					if got := sockoptstest.AcceptedBusyPoll(t, c); got != tc.want {
						t.Errorf("SocketBusyPoll %v on %v: the accepted socket's SO_BUSY_POLL = %d, want %d (baseline %d)",
							tc.d, start.want, got, tc.want, base)
					}
				}
				if active := e.ActiveEngine().Type(); active != start.want {
					t.Fatalf("PREMISE: the engine switched from %v to %v during the test", start.want, active)
				}
			})
		}
	}
}
