//go:build linux

package iouring

import (
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
// io_uring engine, on whichever tier the host and CELERIS_MAX_IOURING_TIER
// give: an accepted socket carries no SO_BUSY_POLL unless
// Resources.SocketBusyPoll asks for it, and then carries exactly the budget
// asked for (37 us, not the 50 us the engine used to set on every socket).
func TestAcceptedSocketBusyPollIsOptIn(t *testing.T) {
	base := sockoptstest.Baseline(t)
	for _, tc := range []struct {
		name string
		d    time.Duration
		want int
	}{
		{"default", 0, base},
		{"37us", 37 * time.Microsecond, 37},
		{"1ms", time.Millisecond, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, cancel, done := startRingRetried662(t, func() (*Engine, error) {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatalf("pick port: %v", err)
				}
				addr := ln.Addr().String()
				_ = ln.Close()
				return New(resource.Config{
					Addr:      addr,
					Engine:    engine.IOUring,
					Protocol:  engine.HTTP1,
					Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
					Resources: resource.Resources{Workers: 2, SocketBusyPoll: tc.d},
				}, respondingHandler{})
			})
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(15 * time.Second):
					t.Error("engine did not stop within 15s")
				}
			})
			for range 4 {
				c, err := net.DialTimeout("tcp", e.Addr().String(), 3*time.Second)
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				t.Cleanup(func() { _ = c.Close() })
				sockoptstest.RoundTrip(t, c)
				if got := sockoptstest.AcceptedBusyPoll(t, c); got != tc.want {
					t.Errorf("SocketBusyPoll %v: the accepted socket's SO_BUSY_POLL = %d, want %d (baseline %d)", tc.d, got, tc.want, base)
				}
			}
		})
	}
}
