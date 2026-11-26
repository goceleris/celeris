//go:build linux

package redis

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/internal/busypolltest"
)

// ctxWatcherStartedModes are the modes that need a platform's own engine: an
// epoll engine that has not started (its standalone loop runs the commands on
// the event loop) and one that has (its command conns run direct, "integrated"),
// and a started io_uring engine where the host runs one (its conns are direct as
// well; a host without io_uring just does not have the mode).
func ctxWatcherStartedModes(t *testing.T) map[string][]Option {
	m := map[string][]Option{
		"engine-not-started-epoll":          {WithEngine(busypolltest.NewServer(0))},
		"engine-started-epoll (integrated)": {WithEngine(busypolltest.StartServer(t, 0))},
	}
	if srv := startIOUringServer(t); srv != nil {
		m["engine-started-io_uring (integrated)"] = []Option{WithEngine(srv)}
	}
	return m
}

// startIOUringServer runs a celeris server on the io_uring engine, with no routes,
// and returns it once it is listening, or nil if this host cannot run one.
func startIOUringServer(t *testing.T) *celeris.Server {
	srv := celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: celeris.IOUring})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- srv.StartWithListener(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for srv.Addr() == nil && time.Now().Before(deadline) {
		select {
		case <-done:
			t.Log("the io_uring engine cannot run on this host: no io_uring mode")
			return nil
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if srv.Addr() == nil || srv.EventLoopProvider() == nil {
		t.Log("the io_uring server did not start: no io_uring mode")
		return nil
	}
	return srv
}
