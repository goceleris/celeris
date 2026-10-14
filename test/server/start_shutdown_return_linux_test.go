//go:build linux

package celeris_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#595: Server.Start / Server.StartWithListener handed Engine.Listen a
// context.Background(); the native engines park Listen on <-ctx.Done() and
// their Engine.Shutdown is a no-op, so Start never returned after
// Server.Shutdown on io_uring and epoll (std returned, adaptive returned via
// its own Listen-cancel). These tests pin, per engine, that
//
//	(a) Start/StartWithListener return within the Shutdown deadline + 1s, and
//	(b) a request in flight when Shutdown is called still completes.
//
// The wait for Start is capped at startWaitCap so the pre-fix negative control
// FAILS instead of hanging the run.
const (
	shutdownBudget = 2 * time.Second
	startSlack     = 1 * time.Second
	startWaitCap   = 5 * time.Second
	inFlightDelay  = 300 * time.Millisecond
)

func startShutdownEngines() []struct {
	name string
	eng  celeris.EngineType
} {
	return []struct {
		name string
		eng  celeris.EngineType
	}{
		{"iouring", celeris.IOUring},
		{"epoll", celeris.Epoll},
		{"std", celeris.Std},
		{"adaptive", celeris.Adaptive},
	}
}

// TestStartWithListenerReturnsAfterShutdown is the reported shape (the
// probatorium refapps call StartWithListener and shut down on SIGTERM).
func TestStartWithListenerReturnsAfterShutdown(t *testing.T) {
	for _, tc := range startShutdownEngines() {
		t.Run(tc.name, func(t *testing.T) {
			runStartShutdownCase(t, tc.eng, true)
		})
	}
}

// TestStartReturnsAfterShutdown covers the plain Start entry point, which had
// the identical context.Background() defect.
func TestStartReturnsAfterShutdown(t *testing.T) {
	for _, tc := range startShutdownEngines() {
		t.Run(tc.name, func(t *testing.T) {
			runStartShutdownCase(t, tc.eng, false)
		})
	}
}

// TestStartReturnsAfterShutdownAsyncIOUring exercises the same guarantee with
// async dispatch on, where the handler runs on a dispatch goroutine and its
// response reaches the ring through the detach queue — the path the io_uring
// shutdown drain inspects under detachMu.
func TestStartReturnsAfterShutdownAsyncIOUring(t *testing.T) {
	runStartShutdownCaseCfg(t, celeris.Config{Engine: celeris.IOUring, AsyncHandlers: true}, true)
}

func runStartShutdownCase(t *testing.T, engType celeris.EngineType, withListener bool) {
	t.Helper()
	runStartShutdownCaseCfg(t, celeris.Config{Engine: engType}, withListener)
}

func runStartShutdownCaseCfg(t *testing.T, cfg celeris.Config, withListener bool) {
	t.Helper()
	engType := cfg.Engine

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	switch {
	case !withListener:
		// Start binds the address itself; hand it the port we just probed.
		cfg.Addr = addr
		if cerr := ln.Close(); cerr != nil {
			t.Fatalf("close probe listener: %v", cerr)
		}
	case engType == celeris.Adaptive:
		// Adaptive + a supplied listener needs Addr set to the listener's
		// own address: adaptive.New re-applies WithDefaults and resolves
		// the resulting ":8080" to "[::]:8080", and the sub-engine's
		// Validate then rejects the pair with `ambiguous configuration:
		// Addr="[::]:8080" but Listener is bound to ...`. Unrelated to
		// celeris#595 — without this line the engine never starts at all.
		cfg.Addr = addr
	}

	handlerStarted := make(chan struct{}, 1)
	s := celeris.New(cfg)
	s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
	s.GET("/slow", func(c *celeris.Context) error {
		select {
		case handlerStarted <- struct{}{}:
		default:
		}
		time.Sleep(inFlightDelay)
		return c.String(http.StatusOK, "done")
	})

	startDone := make(chan error, 1)
	go func() {
		if withListener {
			startDone <- s.StartWithListener(ln)
		} else {
			startDone <- s.Start()
		}
	}()

	base := "http://" + addr
	client := &http.Client{Timeout: 10 * time.Second}
	// Readiness probes get their own short per-request timeout: until the
	// engine is up, the probe listener's backlog still accepts the TCP
	// connection and nobody answers, so a long timeout would park the poll
	// loop and hide a Start that already returned an error.
	if !waitReady(t, &http.Client{Timeout: 300 * time.Millisecond}, base+"/ping", startDone) {
		t.Fatalf("%s: server never became ready on %s", engType, addr)
	}

	type result struct {
		body string
		err  error
	}
	res := make(chan result, 1)
	go func() {
		resp, gerr := client.Get(base + "/slow")
		if gerr != nil {
			res <- result{err: gerr}
			return
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		res <- result{body: string(body)}
	}()

	select {
	case <-handlerStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight handler did not start")
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer shutCancel()
	t0 := time.Now()
	if serr := s.Shutdown(shutCtx); serr != nil {
		t.Errorf("Shutdown: %v", serr)
	}

	select {
	case serr := <-startDone:
		elapsed := time.Since(t0)
		if serr != nil {
			t.Errorf("Start returned error: %v", serr)
		}
		if elapsed > shutdownBudget+startSlack {
			t.Errorf("Start returned after %v, want <= %v (shutdown budget + slack)",
				elapsed, shutdownBudget+startSlack)
		}
		t.Logf("%s: Start returned %v after Shutdown", engType, elapsed)
	case <-time.After(startWaitCap):
		t.Fatalf("celeris#595: Start did not return within %v of Shutdown on %s", startWaitCap, engType)
	}

	select {
	case r := <-res:
		if r.err != nil {
			t.Errorf("in-flight request failed: %v", r.err)
		} else if r.body != "done" {
			t.Errorf("in-flight body = %q, want %q", r.body, "done")
		}
	case <-time.After(5 * time.Second):
		t.Error("in-flight request never completed")
	}
}

// waitReady polls /ping until the engine answers, failing fast if Start
// already returned an error (e.g. the engine is unsupported on this kernel —
// a silent skip would look like a pass).
func waitReady(t *testing.T, client *http.Client, url string, startDone <-chan error) bool {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-startDone:
			t.Fatalf("Start returned before the server was ready: %v", err)
		default:
		}
		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
