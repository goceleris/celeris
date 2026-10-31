//go:build linux

package epoll

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/mono/monotest"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#1178. The engine's timeouts (idle, read, header, write, the HTTP/2
// stream deadline and the protocol detection) are read from the clock of
// internal/mono, which is monotonic: a step of the wall clock neither cuts a
// connection that is making progress nor holds one that is not. The scenarios are
// monotest's, run with the wall reading of the clock stepping by 75 s every 50 ms,
// forward and backward (see its package documentation).
func TestEngineTimeoutsIgnoreAStepOfTheWallClock1178(t *testing.T) {
	monotest.Run(t, func(t *testing.T, cfg resource.Config, h stream.Handler) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("pick port: %v", err)
		}
		cfg.Addr = ln.Addr().String()
		_ = ln.Close()
		e, err := New(cfg, h)
		if err != nil {
			t.Fatalf("epoll engine: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() { errCh <- e.Listen(ctx) }()
		t.Cleanup(func() {
			cancel()
			select {
			case <-errCh:
			case <-time.After(10 * time.Second):
				t.Error("epoll engine did not stop within 10 s")
			}
		})
		for dl := time.Now().Add(10 * time.Second); e.Addr() == nil && time.Now().Before(dl); {
			time.Sleep(10 * time.Millisecond)
		}
		if e.Addr() == nil {
			t.Fatal("engine did not bind")
		}
		return e.Addr().String()
	})
}
