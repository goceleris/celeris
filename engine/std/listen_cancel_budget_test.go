package std

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/protocol/h2/stream"
	"github.com/goceleris/celeris/resource"
)

// heldHandler answers only when the test releases it, whatever its context
// says: the shape of a handler that does not watch for cancellation.
type heldHandler struct {
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
}

func (h *heldHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	h.startOnce.Do(func() { close(h.started) })
	select {
	case <-h.release:
	case <-time.After(20 * time.Second):
		// Safety valve: a failing run must still end rather than hold the
		// test binary until the package timeout.
	}
	return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("done"))
}

// TestListenCancelDrainKeepsTheShutdownBudget pins celeris#753 at the engine.
//
// Server.StartWithContext derives Listen's context from the caller's, so a
// cancel reaches Listen before the watcher's Server.Shutdown (which carries
// Config.ShutdownTimeout) reaches Engine.Shutdown. Listen's cancel branch
// used to start the drain itself with context.Background(), and the drain is
// a sync.Once: the Shutdown that came next with a budget waited in once.Do
// for that unbounded drain, and so did the OnShutdown hooks and the Start
// call, until the last handler returned. The budget has to bound the drain
// whichever call started it.
//
// The order is forced, not raced: Listen's context is cancelled first, and
// Shutdown is called only once the listener refuses connections, which is
// the first thing http.Server.Shutdown does, so Listen's call already holds
// the drain. The handler stays held until every assertion has run, so a
// drain that waits for it cannot end inside the bound, every time.
func TestListenCancelDrainKeepsTheShutdownBudget(t *testing.T) {
	const budget = 300 * time.Millisecond
	// bound is how long after the budget has run out the Shutdown and the
	// Listen calls may take to return. Generous against scheduling noise;
	// the handler is held for longer than bound, until after the checks.
	const bound = 3 * time.Second

	h := &heldHandler{started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(h.release) }) }
	defer releaseHandler()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	e, err := New(resource.Config{
		Listener:     ln,
		Engine:       engine.Std,
		Protocol:     engine.HTTP1,
		ReadTimeout:  -1,
		WriteTimeout: -1,
	}, h)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("New: %v", err)
	}
	listenCtx, cancelListen := context.WithCancel(context.Background())
	defer cancelListen()
	listenDone := make(chan error, 1)
	go func() { listenDone <- e.Listen(listenCtx) }()

	client := &http.Client{Timeout: 30 * time.Second}
	defer client.CloseIdleConnections()
	type result struct {
		body string
		err  error
	}
	respCh := make(chan result, 1)
	go func() {
		var resp *http.Response
		var err error
		for deadline := time.Now().Add(5 * time.Second); ; {
			if resp, err = client.Get("http://" + addr + "/held"); err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			respCh <- result{err: err}
			return
		}
		b, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		respCh <- result{string(b), err}
	}()
	select {
	case <-h.started:
	case r := <-respCh:
		t.Fatalf("request ended before the handler held it: %+v", r)
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start within 5s")
	}

	// Listen's cancel branch starts the drain; wait until the listener is
	// closed, which http.Server.Shutdown does first.
	cancelListen()
	for deadline := time.Now().Add(5 * time.Second); ; {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			break
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the listener still accepted 5s after Listen's context was cancelled")
		}
		time.Sleep(5 * time.Millisecond)
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	start := time.Now()
	shutDone := make(chan error, 1)
	go func() { shutDone <- e.Shutdown(shutCtx) }()
	select {
	case err := <-shutDone:
		if el := time.Since(start); el < budget {
			t.Errorf("Shutdown returned after %v, before its %v budget ran out, with the handler still running", el, budget)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Shutdown: got %v, want context.DeadlineExceeded: its budget ran out with the handler still running", err)
		}
	case <-time.After(budget + bound):
		t.Fatalf("Shutdown with a %v budget had not returned %v after it began: it waited for the drain Listen's cancel started with no budget (celeris#753)", budget, budget+bound)
	}
	select {
	case err := <-listenDone:
		if err != nil {
			t.Errorf("Listen: %v, want nil", err)
		}
	case <-time.After(bound):
		t.Fatalf("Listen had not returned %v after the Shutdown budget ran out: its drain ignores the budget (celeris#753)", bound)
	}

	// The budget ends the drain, not the request: the connection is left
	// to finish, as http.Server.Shutdown leaves it.
	releaseHandler()
	select {
	case r := <-respCh:
		if r.err != nil || r.body != "done" {
			t.Errorf("the held request answered %q, %v; want \"done\"", r.body, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the held request did not complete within 5s of its release")
	}
}
