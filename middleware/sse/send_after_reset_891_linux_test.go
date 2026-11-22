//go:build linux

package sse_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/goceleris/celeris/middleware/sse"
)

// TestSendAfterPeerResetReturnsAnError891 pins the contract the Client.Send and
// Client.WritePreparedEvent godoc state (celeris#891, umbrella item 168): on the
// native engines over HTTP/1.1, once the client has reset the connection, a
// handler that keeps writing sees a non-nil error from the write, an error that
// wraps net.ErrClosed (the engine's closed-connection report, celeris#1067) or
// the stream context's cancellation (Client.Context, which a reset also
// cancels). Before #1067 such a write was dropped without an error; the godoc
// still said so.
//
// The handler ignores Client.Context on purpose and writes until a write fails,
// so the error it records is the one a caller that never looks at the context
// gets.
func TestSendAfterPeerResetReturnsAnError891(t *testing.T) {
	type writer struct {
		name  string
		write func(*sse.Client, int) error
	}
	prepared := sse.NewPreparedEvent(sse.Event{Event: "tick", Data: "p"})
	writers := []writer{
		{"Send", func(c *sse.Client, n int) error {
			return c.Send(sse.Event{Event: "tick", Data: fmt.Sprintf("%d", n)})
		}},
		{"WritePreparedEvent", func(c *sse.Client, _ int) error { return c.WritePreparedEvent(prepared) }},
	}
	for _, kind := range sseNativeEngineKinds(t) {
		for _, async := range []bool{false, true} {
			for _, w := range writers {
				name := fmt.Sprintf("%s/async=%v/%s", kind, async, w.name)
				t.Run(name, func(t *testing.T) {
					type outcome struct {
						err      error
						ctxErr   error
						accepted int
					}
					out := make(chan outcome, 1)
					handler := sse.New(sse.Config{
						HeartbeatInterval: -1,
						Handler: func(client *sse.Client) {
							accepted := 0
							for {
								if err := w.write(client, accepted); err != nil {
									out <- outcome{err: err, ctxErr: client.Context().Err(), accepted: accepted}
									return
								}
								accepted++
								time.Sleep(5 * time.Millisecond)
							}
						},
					})
					addr, shutdown := startSSEServer(t, kind, async, false, handler)
					defer shutdown()

					conn, br := openSSEStream(t, addr)
					defer func() { _ = conn.Close() }()
					readTicks(t, br, 2)

					tc := conn.(*net.TCPConn)
					if err := tc.SetLinger(0); err != nil { // Close sends an RST
						t.Fatalf("SetLinger: %v", err)
					}
					_ = tc.Close()

					select {
					case o := <-out:
						if o.err == nil {
							t.Fatal("write returned a nil error")
						}
						if !errors.Is(o.err, net.ErrClosed) && !errors.Is(o.err, context.Canceled) {
							t.Fatalf("write after a reset returned %v, want an error wrapping net.ErrClosed or context.Canceled", o.err)
						}
						t.Logf("write %d after the reset failed with %v (Context().Err() = %v)", o.accepted, o.err, o.ctxErr)
					case <-time.After(10 * time.Second):
						t.Fatal("the handler's writes still succeeded 10s after the client reset the connection: the write after a reset is dropped without an error")
					}
				})
			}
		}
	}
}
