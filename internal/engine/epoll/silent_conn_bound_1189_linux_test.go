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

// celeris#1189. A connection that sends nothing is closed at ReadHeaderTimeout from
// the engine's accept: no later than the 25 ms sweep of the timerfd after it, and,
// with TCP_DEFER_ACCEPT on (the default), the kernel's deferral of a silent connection
// (about 1 s) is added before the accept. See monotest.SilentConnIsCutAtReadHeaderTimeout.
func TestSilentConnIsCutAtReadHeaderTimeout1189(t *testing.T) {
	monotest.SilentConnIsCutAtReadHeaderTimeout(t, startSilent1189)
}

func TestSilentConnIsCutAfterDeferAccept1189(t *testing.T) {
	monotest.SilentConnIsCutAfterDeferAccept(t, startSilent1189)
}

func startSilent1189(t *testing.T, cfg resource.Config, h stream.Handler) string {
	t.Helper()
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
}
