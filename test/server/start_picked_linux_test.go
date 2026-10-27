//go:build linux

package celeris_test

import (
	"context"
	"fmt"
	"net"
	"time"

	"testing"

	"github.com/goceleris/celeris"
)

// listenForStart1104 gives startPicked1104 the listener it picks its port with.
// A variable so a test can make two starters pick the same port, which two
// parallel test cells of one process do now and then: net.Listen on ":0" hands a
// port out again as soon as it is closed.
var listenForStart1104 = func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

// startPicked1104 builds a server with cfg (Addr is set here) and routes, starts
// it on a port it picked, and returns it with its address and Start's result
// channel once ready reports it up. A ring ENOMEM is retried for the window
// (startRetryingRingENOMEM; a fresh server and port per try) and a start that
// fails otherwise fails t. ready is given the server and Start's result channel
// and must return Start's error if it comes first.
func startPicked1104(t testing.TB, cfg celeris.Config, routes func(*celeris.Server), ready func(s *celeris.Server, addr string, done <-chan error) error) (*celeris.Server, string, chan error) {
	t.Helper()
	var (
		s    *celeris.Server
		addr string
		done chan error
	)
	startRetryingRingENOMEM(t, "server start", func() error {
		ln, err := listenForStart1104()
		if err != nil {
			t.Fatal(err)
		}
		addr = ln.Addr().String()
		_ = ln.Close()
		cfg.Addr = addr
		s = celeris.New(cfg)
		routes(s)
		done = make(chan error, 1)
		go func(s *celeris.Server, done chan error) { done <- s.Start() }(s, done)
		if err := ready(s, addr, done); err != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.Shutdown(ctx)
			cancel()
			return fmt.Errorf("Start returned before the server was ready: %w", err)
		}
		return nil
	})
	return s, addr, done
}
