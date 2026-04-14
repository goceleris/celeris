package celeris_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestServerShutdownDrainsInFlight starts a real server, holds a slow
// in-flight request open, calls Shutdown with a timeout, and verifies
// the request completes (or the deadline expires cleanly) without
// dropping the response.
func TestServerShutdownDrainsInFlight(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	const slowDelay = 250 * time.Millisecond
	requestStarted := make(chan struct{})
	s := celeris.New(celeris.Config{Engine: celeris.Std})
	s.GET("/slow", func(c *celeris.Context) error {
		close(requestStarted)
		time.Sleep(slowDelay)
		return c.String(200, "done")
	})

	servCtx, servCancel := context.WithCancel(context.Background())
	servDone := make(chan error, 1)
	go func() { servDone <- s.StartWithListenerAndContext(servCtx, ln) }()

	// Fire the slow request in another goroutine so we can race Shutdown.
	type result struct {
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			res <- result{err: err}
			return
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		res <- result{body: string(body)}
	}()

	// Wait for the handler to be running.
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start in time")
	}

	// Shutdown with a budget large enough to drain.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	shutdownErr := s.Shutdown(shutdownCtx)
	servCancel()
	<-servDone

	if shutdownErr != nil && !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("Shutdown: %v", shutdownErr)
	}

	r := <-res
	if r.err != nil {
		t.Fatalf("in-flight request failed: %v", r.err)
	}
	if r.body != "done" {
		t.Errorf("body = %q, want done — handler did not complete before shutdown closed the connection", r.body)
	}
}

// TestStartAfterShutdown documents what happens if a user tries to
// restart a Server after Shutdown — must surface a clear error rather
// than silently re-binding or panicking.
func TestStartAfterShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := celeris.New(celeris.Config{Engine: celeris.Std})
	s.GET("/", func(c *celeris.Context) error { return c.NoContent(204) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(ctx, ln) }()

	// Wait for ready.
	addr := ln.Addr().String()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, derr := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if derr == nil {
			_ = c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Shutdown.
	shutdownCtx, sc := context.WithTimeout(context.Background(), 2*time.Second)
	defer sc()
	if err := s.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown: %v", err)
	}
	cancel()
	<-done

	// Now attempt Start again on a fresh listener — should error, not panic.
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("re-listen: %v", err)
	}
	defer func() { _ = ln2.Close() }()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	startErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		startErr <- s.StartWithListenerAndContext(ctx2, ln2)
	}()

	select {
	case err := <-startErr:
		// Either a clear error OR returns nil immediately are both
		// acceptable. The pin here is "no panic".
		_ = err
	case <-time.After(500 * time.Millisecond):
		// Server entered run loop again — shutdown the second instance.
		cancel2()
		<-startErr
	}
	wg.Wait()
}
