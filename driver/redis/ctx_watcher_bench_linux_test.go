//go:build linux

package redis_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/redis"
)

type benchMode = struct {
	name string
	opts []redis.Option
}

func benchStarted(b *testing.B, et celeris.EngineType) *celeris.Server {
	srv := celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: et})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.StartWithListener(ln) }()
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	for i := 0; i < 2000 && srv.Addr() == nil; i++ {
		select {
		case <-done:
			return nil
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if srv.Addr() == nil {
		return nil
	}
	return srv
}

// benchPlatformModes are the integrated modes: a started epoll and a started
// io_uring engine (left out where the host cannot run one).
func benchPlatformModes(b *testing.B) []benchMode {
	var m []benchMode
	if srv := benchStarted(b, celeris.Epoll); srv != nil {
		m = append(m, benchMode{"integrated-epoll", []redis.Option{redis.WithEngine(srv)}})
	}
	if srv := benchStarted(b, celeris.IOUring); srv != nil {
		m = append(m, benchMode{"integrated-io_uring", []redis.Option{redis.WithEngine(srv)}})
	}
	return m
}
