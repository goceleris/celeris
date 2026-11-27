//go:build linux

package postgres

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// A Pool opened WithEngine on a server whose Config.Engine is unset and that has
// an .Async() route dials direct connections (Go's netpoll, nothing on a loop),
// before the server starts and, the same Pool, after it (celeris#1376). Before
// the fix the unset engine reported no async handlers, and the Pool opened before
// Start ran on the driver's own loop. On Linux only: elsewhere the driver's loop
// has no synchronous path and the Pool dials direct anyway.
func TestPoolDefaultEngineAsyncServerDialsDirect1376(t *testing.T) {
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 42, 0x1ABCD123, func(c net.Conn) { _, _ = io.Copy(io.Discard, c) })
	})
	host, port, _ := net.SplitHostPort(addr)
	srv := celeris.New(celeris.Config{Addr: "127.0.0.1:0"}) // Engine unset
	srv.GET("/x", func(c *celeris.Context) error { return c.String(200, "ok") }).Async()
	p, err := Open("postgres://u@"+host+":"+port+"/d?sslmode=disable", WithMaxOpen(1), WithEngine(srv))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	t.Cleanup(func() { // the Pool first, then the server
		_ = p.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = ln.Close()
	})
	dials := func(when string) {
		t.Helper()
		c, err := p.acquire(context.Background())
		if err != nil {
			t.Fatalf("%s: acquire: %v", when, err)
		}
		direct := c.useDirect && c.tcp != nil && c.loop == nil
		p.inner.Discard(c) // the next acquire dials again
		if !direct {
			t.Errorf("%s: the Pool's connection is not direct (useDirect=%v)", when, c.useDirect)
		}
	}
	dials("before Start")

	go func() { done <- srv.StartWithListener(ln) }()
	for deadline := time.Now().Add(10 * time.Second); srv.EventLoopProvider() == nil; time.Sleep(5 * time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("the server did not start: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the server has no event-loop provider 10 s after Start")
		}
	}
	dials("after Start")
}
