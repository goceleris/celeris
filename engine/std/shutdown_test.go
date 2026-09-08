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

// parkedHandler is the std-engine shape of a detached stream: an SSE
// handler with heartbeats disabled runs inline in ServeHTTP and parks on
// its request context, so nothing but engine-driven cancellation can wake
// it. It records when it started and when its context was cancelled.
type parkedHandler struct {
	started   chan struct{}
	ctxDone   chan struct{}
	startOnce sync.Once
	doneOnce  sync.Once
}

func (h *parkedHandler) HandleStream(ctx context.Context, s *stream.Stream) error {
	h.startOnce.Do(func() { close(h.started) })
	select {
	case <-ctx.Done():
		h.doneOnce.Do(func() { close(h.ctxDone) })
	case <-time.After(20 * time.Second):
		// Safety valve: a failing run must still terminate rather than
		// wedge the test binary until the package timeout.
	}
	return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("parked"))
}

// startEngine boots the std engine on a fresh loopback listener and
// returns its address plus a stop closure. ReadTimeout/WriteTimeout are
// disabled: a streaming deployment must disable them anyway, and leaving
// the 60s defaults in place would let net/http's read deadline cancel the
// request context for reasons unrelated to shutdown.
func startEngine(t *testing.T, h stream.Handler) (*Engine, string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
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
	listenDone := make(chan error, 1)
	go func() { listenDone <- e.Listen(listenCtx) }()

	deadline := time.Now().Add(5 * time.Second)
	for e.Addr() == nil {
		if time.Now().After(deadline) {
			cancelListen()
			t.Fatal("listener not ready within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	return e, e.Addr().String(), func() {
		cancelListen()
		select {
		case <-listenDone:
		case <-time.After(5 * time.Second):
			t.Log("Listen did not return within 5s")
		}
	}
}

// TestShutdownCancelsStuckRequestContext pins the celeris#498 follow-up.
//
// http.Server.Shutdown waits for connections to go idle; it cancels
// nothing. A detached stream on std (SSE with heartbeats off, parked on
// client.Context()) runs inline in ServeHTTP, so its connection never
// goes idle, Shutdown burns its whole drain budget and returns
// DeadlineExceeded — and the handler goroutine, its request context and
// the connection are still there afterwards, for the lifetime of the
// process. The drain budget must also bound how long a request context
// stays live: once it is spent, the base context is cancelled so those
// handlers are woken and unwind cooperatively.
func TestShutdownCancelsStuckRequestContext(t *testing.T) {
	h := &parkedHandler{started: make(chan struct{}), ctxDone: make(chan struct{})}
	e, addr, stop := startEngine(t, h)
	defer stop()

	client := &http.Client{Timeout: 15 * time.Second}
	defer client.CloseIdleConnections()
	respCh := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://" + addr + "/events")
		if err != nil {
			respCh <- err
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		respCh <- resp.Body.Close()
	}()

	select {
	case <-h.started:
	case err := <-respCh:
		t.Fatalf("request finished before the handler parked: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start within 5s")
	}

	const budget = 300 * time.Millisecond
	shutCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	err := e.Shutdown(shutCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown: got %v, want DeadlineExceeded — the parked handler should have held the drain open", err)
	}

	select {
	case <-h.ctxDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("request context still live 3s after Shutdown spent its %v drain budget: "+
			"a detached stream on std is woken by nothing else, so its handler goroutine, "+
			"context and connection leak past shutdown — celeris#498", budget)
	}
}

// slowHandler answers after delay, reporting whether its request context
// was still live when it finished.
type slowHandler struct {
	delay     time.Duration
	started   chan struct{}
	startOnce sync.Once
	ctxErr    error
}

func (h *slowHandler) HandleStream(ctx context.Context, s *stream.Stream) error {
	h.startOnce.Do(func() { close(h.started) })
	time.Sleep(h.delay)
	h.ctxErr = ctx.Err()
	return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("done"))
}

// TestShutdownDrainsInFlightRequest is the guard on the other half of the
// contract: escalation happens when the drain budget is *spent*, never
// while it still holds. An ordinary in-flight request must keep a live
// context, complete normally and have its response delivered. (This one
// passes both before and after the fix — it is a regression guard, not
// the failing pin above.)
func TestShutdownDrainsInFlightRequest(t *testing.T) {
	h := &slowHandler{delay: 250 * time.Millisecond, started: make(chan struct{})}
	e, addr, stop := startEngine(t, h)
	defer stop()

	client := &http.Client{Timeout: 15 * time.Second}
	defer client.CloseIdleConnections()
	type result struct {
		body string
		err  error
	}
	respCh := make(chan result, 1)
	go func() {
		resp, err := client.Get("http://" + addr + "/slow")
		if err != nil {
			respCh <- result{err: err}
			return
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		respCh <- result{body: string(body), err: err}
	}()

	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start within 5s")
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Shutdown(shutCtx); err != nil {
		t.Fatalf("Shutdown: %v, want nil — the request was well within the drain budget", err)
	}

	select {
	case r := <-respCh:
		if r.err != nil {
			t.Fatalf("in-flight request failed: %v", r.err)
		}
		if r.body != "done" {
			t.Errorf("body = %q, want %q", r.body, "done")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not complete within 5s of Shutdown returning")
	}
	if h.ctxErr != nil {
		t.Errorf("request context was cancelled mid-handler during a graceful drain: %v", h.ctxErr)
	}
}
