//go:build linux

package sse_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// waitUpOrStopped waits, after the caller's settle sleep, until srv
// publishes the address it serves on or its Start returns (done is closed),
// and fails the test if neither happens within timeout. The caller then
// reads Start's error as before. It replaces a single read of that error
// 500 ms in, which missed a Start that failed later and went on to drive a
// server that was not there (celeris#706).
func waitUpOrStopped(tb testing.TB, srv *celeris.Server, done <-chan struct{}, timeout time.Duration) {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for srv.Addr() == nil {
		select {
		case <-done:
			return
		default:
		}
		if !time.Now().Before(deadline) {
			tb.Fatalf("server neither published its address nor returned from Start within %v", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestWaitUpOrStoppedReturnsWhenStartFails pins waitUpOrStopped on a Start
// that returns before the server is up (celeris#706): it returns as soon as
// Start does, so the caller reads Start's error at once. The engine type is
// one no engine factory knows, so Start fails at once.
func TestWaitUpOrStoppedReturnsWhenStartFails(t *testing.T) {
	srv := celeris.New(celeris.Config{Engine: celeris.EngineType(250)})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	defer func() { _ = srv.Shutdown(context.Background()) }()
	var startErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		startErr = srv.StartWithListenerAndContext(context.Background(), ln)
	}()
	start := time.Now()
	waitUpOrStopped(t, srv, done, 30*time.Second)
	elapsed := time.Since(start)
	<-done
	t.Logf("MW706WAIT helper=sse_test.waitUpOrStopped after=%v startErr=%v", elapsed.Round(time.Millisecond), startErr)
	if startErr == nil || !strings.Contains(startErr.Error(), "unknown engine type") || elapsed > 5*time.Second {
		t.Fatalf("waitUpOrStopped returned after %v with Start's error %v; want Start's own error (unknown engine type), as soon as Start returns", elapsed.Round(time.Millisecond), startErr)
	}
}
