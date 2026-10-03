package celeris

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitServerStarted waits until s, whose Start runs in a goroutine that sends
// Start's error on done, accepts connections at its own address, and returns
// that address.
//
// A Start that returns before the server is ready means it never will be, so
// waitServerStarted fails at once with Start's error instead of polling out
// the timeout and reporting a bare "not ready" that reads the same as a hang
// (celeris#706). It puts the error back for the caller's shutdown, which may
// receive from done too: done must be buffered, and Start sends on it once.
// It probes s.Addr(), which only a server that started reports, rather than
// the listener the caller handed over: the engines close and rebind that
// listener, and a Start that fails before any engine runs closes it
// (celeris#737), so a dial of it fails until the deadline and says nothing
// about why.
func waitServerStarted(tb testing.TB, s *Server, done chan error, timeout time.Duration) string {
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

// fatalRecorder706 is a testing.TB whose Fatal and Fatalf record the message
// and end the calling goroutine, as t.Fatal ends a test's, without failing
// the test that owns it. Everything else goes to the embedded TB.
type fatalRecorder706 struct {
	testing.TB
	mu     sync.Mutex
	msg    string
	failed bool
}

func (f *fatalRecorder706) Fatal(args ...any) { f.record(fmt.Sprint(args...)) }

func (f *fatalRecorder706) Fatalf(format string, args ...any) {
	f.record(fmt.Sprintf(format, args...))
}

func (f *fatalRecorder706) record(msg string) {
	f.mu.Lock()
	f.msg, f.failed = msg, true
	f.mu.Unlock()
	runtime.Goexit()
}

// TestWaitServerStartedFailsFastOnStartError pins waitServerStarted's
// handling of a Start that returns before the server is ready (celeris#706):
// it fails at once with Start's error, and leaves that error in done for the
// caller's shutdown. The engine type is one no engine factory knows, so
// Start fails at once with "unknown engine type".
func TestWaitServerStartedFailsFastOnStartError(t *testing.T) {
	s := New(Config{Engine: EngineType(250)})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	defer func() { _ = s.Shutdown(context.Background()) }()
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(context.Background(), ln) }()

	rec := &fatalRecorder706{TB: t}
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
	t.Logf("MW706WAIT helper=waitServerStarted failed=%v after=%v msg=%q", failed, elapsed.Round(time.Millisecond), msg)
	if !failed {
		t.Fatal("waitServerStarted reported ready a server whose Start failed")
	}
	if !strings.Contains(msg, "unknown engine type") || elapsed > 5*time.Second {
		t.Fatalf("waitServerStarted failed after %v with %q; want Start's own error (unknown engine type), reported as soon as Start returns", elapsed.Round(time.Millisecond), msg)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "unknown engine type") {
			t.Fatalf("done holds %v after waitServerStarted, want Start's error", err)
		}
	default:
		t.Fatal("waitServerStarted consumed Start's error: a caller's shutdown receiving from done would block for good")
	}
}
