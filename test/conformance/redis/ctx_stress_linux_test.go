//go:build redis && linux

package redis_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	celredis "github.com/goceleris/celeris/driver/redis"
)

func stressPlatformModes(t *testing.T) map[string][]celredis.Option {
	m := map[string][]celredis.Option{
		"engine-not-started-epoll": {celredis.WithEngine(celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: celeris.Epoll}))},
	}
	if srv := startServer(t, celeris.Epoll); srv != nil {
		m["engine-started-epoll (integrated)"] = []celredis.Option{celredis.WithEngine(srv)}
	}
	if srv := startServer(t, celeris.IOUring); srv != nil {
		m["engine-started-io_uring (integrated)"] = []celredis.Option{celredis.WithEngine(srv)}
	}
	return m
}

// startServer runs a celeris server on engine et, on a loopback listener and with
// no routes, and returns it once it is listening; nil when this host cannot run
// that engine (the mode is then left out).
func startServer(t *testing.T, et celeris.EngineType) *celeris.Server {
	srv := celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: et})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		_ = srv.StartWithListener(ln)
		close(done)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	for i := 0; i < 2000 && srv.Addr() == nil; i++ {
		select {
		case <-done:
			t.Logf("the %v engine cannot run on this host: no such mode", et)
			return nil
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if srv.Addr() == nil || srv.EventLoopProvider() == nil {
		t.Logf("the %v server did not start: no such mode", et)
		return nil
	}
	return srv
}
