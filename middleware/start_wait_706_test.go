package middleware_test

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// waitServerStarted is the middleware tests' twin of the root helper of
// the same name (server_start_wait_706_test.go): it waits until s accepts
// connections at its own address and returns it, failing at once with
// Start's error if Start returns first and putting that error back in done,
// which must be buffered (celeris#706). It probes s.Addr(), which only a
// server that started reports, not the caller's listener.
func waitServerStarted(tb testing.TB, s *celeris.Server, done chan error, timeout time.Duration) string {
	tb.Helper()
	deadline := time.Now().Add(timeout)
	for {
		select {
		case err := <-done:
			done <- err
			tb.Fatalf("server stopped before it was ready: Start returned %v", err)
			return ""
		default:
		}
		if a := s.Addr(); a != nil {
			if c, err := net.DialTimeout("tcp", a.String(), 100*time.Millisecond); err == nil {
				_ = c.Close()
				return a.String()
			}
		}
		if !time.Now().Before(deadline) {
			tb.Fatalf("server not ready within %v, and Start has not returned", timeout)
			return ""
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fatalRecorderMW706 is a testing.TB whose Fatal and Fatalf record the
// message and end the calling goroutine without failing the owning test.
type fatalRecorderMW706 struct {
	testing.TB
	mu     sync.Mutex
	msg    string
	failed bool
}

func (f *fatalRecorderMW706) Fatal(args ...any) { f.record(fmt.Sprint(args...)) }

func (f *fatalRecorderMW706) Fatalf(format string, args ...any) {
	f.record(fmt.Sprintf(format, args...))
}

func (f *fatalRecorderMW706) record(msg string) {
	f.mu.Lock()
	f.msg, f.failed = msg, true
	f.mu.Unlock()
	runtime.Goexit()
}

// TestWaitServerStartedMiddlewareFailsFastOnStartError is the root
// TestWaitServerStartedFailsFastOnStartError for this package's twin.
func TestWaitServerStartedMiddlewareFailsFastOnStartError(t *testing.T) {
	s := celeris.New(celeris.Config{Engine: celeris.EngineType(250)})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	defer func() { _ = s.Shutdown(context.Background()) }()
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(context.Background(), ln) }()

	rec := &fatalRecorderMW706{TB: t}
	start := time.Now()
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		waitServerStarted(rec, s, done, 30*time.Second)
	}()
	select {
	case <-exited:
	case <-time.After(90 * time.Second):
		t.Fatal("harness: waitServerStarted neither returned nor failed within 90 s")
	}
	elapsed := time.Since(start)
	rec.mu.Lock()
	msg, failed := rec.msg, rec.failed
	rec.mu.Unlock()
	t.Logf("MW706WAIT helper=middleware_test.waitServerStarted failed=%v after=%v msg=%q", failed, elapsed.Round(time.Millisecond), msg)
	if !failed || !strings.Contains(msg, "unknown engine type") || elapsed > 5*time.Second {
		t.Fatalf("waitServerStarted: failed=%v after %v with %q; want a failure with Start's own error (unknown engine type) as soon as Start returns", failed, elapsed.Round(time.Millisecond), msg)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "unknown engine type") {
			t.Fatalf("done holds %v after waitServerStarted, want Start's error", err)
		}
	default:
		t.Fatal("waitServerStarted consumed Start's error")
	}
}
