//go:build linux

package celeris_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// listenForStart1104 gives startPicked1104 the listener it picks its port with.
// A variable so a test can make two starters pick the same port, which two
// parallel test cells of one process do now and then: net.Listen on ":0" hands a
// port out again as soon as it is closed.
var listenForStart1104 = func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }

// startPicked1104 builds a server with cfg (Addr is set here) and routes, starts
// it on a listener it picked, and returns it with its address and Start's result
// channel once ready reports it up. The listener is handed to the server
// (StartWithListener). On the std engine, which serves on that listener, the
// port is never free between the pick and the bind: another cell picking the
// same one gets a different port, and a readiness probe can only be answered by
// this server. The epoll, io_uring and adaptive engines close the supplied
// listener inside Listen and rebind its address with SO_REUSEPORT, so there the
// hand-off only shortens the window to the close and the rebind, and two native
// servers that both win the port both bind it, without an error, and share it
// (see TestStartersForcedOntoOnePortEndOnPortsOfTheirOwn1104). A ring ENOMEM is
// retried for the window (startRetrying1104; a fresh server and port per try)
// and a start that fails otherwise fails t. ready is given the server, its address and Start's result
// channel and must return Start's error if it comes first.
func startPicked1104(t testing.TB, cfg celeris.Config, routes func(*celeris.Server), ready func(s *celeris.Server, addr string, done <-chan error) error) (*celeris.Server, string, chan error) {
	t.Helper()
	var (
		s    *celeris.Server
		addr string
		done chan error
	)
	startRetrying1104(t, "server start", func() error {
		ln, err := listenForStart1104()
		if err != nil {
			t.Fatal(err)
		}
		addr = ln.Addr().String()
		cfg.Addr = addr // the adaptive engine wants Addr to be its listener's address
		s = celeris.New(cfg)
		routes(s)
		done = make(chan error, 1)
		go func(s *celeris.Server, ln net.Listener, done chan error) { done <- s.StartWithListener(ln) }(s, ln, done)
		if err := ready(s, addr, done); err != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.Shutdown(ctx)
			cancel()
			_ = ln.Close()
			return fmt.Errorf("Start returned before the server was ready: %w", err)
		}
		return nil
	})
	return s, addr, done
}

// startPickedPing1104 is startPicked1104 for the starters of this package that
// register their routes (and a /ping, in whatever order they need) and wait for
// /ping to answer: it also registers the cleanup they all had, a Shutdown of the
// server and a wait for Start to return.
func startPickedPing1104(t testing.TB, cfg celeris.Config, routes func(*celeris.Server)) (*celeris.Server, string) {
	t.Helper()
	s, addr, done := startPicked1104(t, cfg, routes, func(_ *celeris.Server, addr string, done <-chan error) error { return waitReady761(addr, done) })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Errorf("Start did not return within 15s of Shutdown")
		}
	})
	return s, addr
}

// ping1104 registers the /ping route startPickedPing1104's readiness probe uses.
func ping1104(s *celeris.Server) {
	s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
}
