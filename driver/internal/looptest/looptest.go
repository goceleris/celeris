// Package looptest gives a driver's test the two servers whose event loop a
// driver cannot use, so that a Client or Pool opened WithEngine of one runs on
// the driver's own standalone loop and has to give it back on Close
// (celeris#1238). The helpers build on every platform.
package looptest

import (
	"context"
	"net"
	"testing"
	"time"

	celeris "github.com/goceleris/celeris"
)

// Unstarted returns a server that has not been started: it has no event loop
// yet, and a driver opened WithEngine of it runs on its own.
func Unstarted() *celeris.Server {
	return celeris.New(celeris.Config{Addr: "127.0.0.1:0"})
}

// StartStd runs a celeris server on the std engine, which has no event loop of
// its own, and without AsyncHandlers, so it is not async either. It returns the
// server once it is listening and shuts it down when the test ends.
func StartStd(t testing.TB) *celeris.Server {
	t.Helper()
	srv := celeris.New(celeris.Config{Addr: "127.0.0.1:0", Engine: celeris.Std})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
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
		case err := <-done:
			t.Fatalf("the std server did not start: %v", err)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if srv.Addr() == nil {
		t.Fatal("the server never bound within 10 s")
	}
	if srv.EventLoopProvider() != nil {
		t.Fatal("the std server has an EventLoopProvider")
	}
	return srv
}
