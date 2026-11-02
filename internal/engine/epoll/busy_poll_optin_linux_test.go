//go:build linux

package epoll

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	celerisengine "github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/resource"
	"github.com/goceleris/celeris/internal/sockopts/sockoptstest"
)

// TestAcceptedSocketBusyPollIsOptIn is celeris#1036 on the
// epoll engine: an accepted socket carries no SO_BUSY_POLL unless
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
			addr := startBusyPollEngine(t, tc.d)
			// Several connections, so that every worker that accepts one is
			// covered, and the one read is not the first one to a warm engine.
			for range 4 {
				c, err := net.DialTimeout("tcp", addr, 3*time.Second)
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

func startBusyPollEngine(t *testing.T, busyPoll time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	eng, err := New(resource.Config{
		Addr:      addr,
		Protocol:  celerisengine.HTTP1,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Resources: resource.Resources{Workers: 2, SocketBusyPoll: busyPoll},
	}, respondingHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Listen(ctx) }()
	if err := awaitBound(eng, done, 5*time.Second); err != nil {
		cancel()
		<-done
		t.Fatalf("engine never bound: %v", err)
	}
	t.Cleanup(func() { cancel(); <-done })
	return addr
}
