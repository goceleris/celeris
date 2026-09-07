package std_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
)

// TestSSEStreamUnwindsOnShutdown is the celeris#498 pin at the level the
// bug was reported at: a real SSE stream, driven through the public API.
//
// The handler here is the bench-refapp shape — heartbeats disabled, three
// events and then parked on client.Context() — so nothing but the engine
// can wake it. On std that stream runs inline in ServeHTTP, so its
// connection never goes idle and http.Server.Shutdown, which only waits
// for connections and cancels nothing, used to spend its whole budget and
// leave the handler goroutine, its context and the connection running for
// the lifetime of the process. Engine.Shutdown now cancels the base
// context once the drain budget is spent, so the handler unwinds shortly
// after ShutdownTimeout and Listen returns.
//
// Read/WriteTimeout are disabled the way a streaming deployment has to
// disable them: with the 60s defaults net/http's read deadline eventually
// cancels the request context on its own, which would mask what is being
// pinned here.
//
// The path under test is also the racy one: on ctx cancellation
// Server.StartWithContext's helper goroutine shuts down with
// ShutdownTimeout while Engine.Listen shuts down with a background
// context, and either can win the once. Both must honour the budget.
func TestSSEStreamUnwindsOnShutdown(t *testing.T) {
	const shutdownTimeout = 500 * time.Millisecond

	var started, returned atomic.Int32
	handler := sse.New(sse.Config{
		HeartbeatInterval: -1,
		Handler: func(client *sse.Client) {
			started.Add(1)
			defer returned.Add(1)
			for n := 1; n <= 3; n++ {
				if err := client.Send(sse.Event{Event: "tick", Data: fmt.Sprintf("%d", n)}); err != nil {
					return
				}
			}
			<-client.Context().Done()
		},
	})

	s := celeris.New(celeris.Config{
		Engine:          celeris.Std,
		ShutdownTimeout: shutdownTimeout,
		ReadTimeout:     -1,
		WriteTimeout:    -1,
	})
	s.GET("/events", handler)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	serverCtx, serverCancel := context.WithCancel(context.Background())
	defer serverCancel()
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.StartWithListenerAndContext(serverCtx, ln) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		c, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if dialErr == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server not ready within 5s: %v", dialErr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET /events HTTP/1.1\r\nHost: %s\r\nAccept: text/event-stream\r\n\r\n", addr); err != nil {
		t.Fatalf("write request: %v", err)
	}
	br := bufio.NewReader(conn)
	for seen := 0; seen < 3; {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read after %d ticks: %v", seen, err)
		}
		if strings.HasPrefix(line, "event: tick") {
			seen++
		}
	}
	if got := started.Load(); got != 1 {
		t.Fatalf("started=%d handlers, want 1", got)
	}

	start := time.Now()
	serverCancel()

	// Generous slack over ShutdownTimeout: the assertion is "bounded by
	// the drain budget", not the exact wakeup latency.
	deadline = time.Now().Add(6 * time.Second)
	for returned.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if returned.Load() == 0 {
		t.Fatalf("SSE handler still parked %v after shutdown started (ShutdownTimeout=%v): "+
			"nothing cancelled its request context, so the handler goroutine, the context and "+
			"the connection outlive the server — celeris#498", time.Since(start), shutdownTimeout)
	}

	select {
	case <-serverDone:
	case <-time.After(10 * time.Second):
		t.Fatal("server goroutine did not return within 10s of shutdown")
	}
}
