//go:build linux

package static

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

// fatalRecorder is a testing.TB whose Fatal and Fatalf record the message and
// end the calling goroutine, as t.Fatal ends a test's, without failing the
// test that owns it.
type fatalRecorder struct {
	testing.TB
	mu     sync.Mutex
	msg    string
	failed bool
}

func (f *fatalRecorder) Fatal(args ...any) { f.record(fmt.Sprint(args...)) }

func (f *fatalRecorder) Fatalf(format string, args ...any) { f.record(fmt.Sprintf(format, args...)) }

func (f *fatalRecorder) record(msg string) {
	f.mu.Lock()
	f.msg, f.failed = msg, true
	f.mu.Unlock()
	runtime.Goexit()
}

// TestWaitForReadyFailsFastOnStartError pins waitForReady's handling of a
// Start that returns before the server is ready (celeris#706): it must fail at
// once with Start's error, not poll out its timeout. The engine type is one no
// engine factory knows, so Start fails at once with "unknown engine type".
func TestWaitForReadyFailsFastOnStartError(t *testing.T) {
	s := celeris.New(celeris.Config{Engine: celeris.EngineType(250)})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	done := make(chan error, 1)
	go func() { done <- s.StartWithListenerAndContext(context.Background(), ln) }()

	rec := &fatalRecorder{TB: t}
	start := time.Now()
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		waitForReady(rec, s, done, 30*time.Second)
	}()
	select {
	case <-exited:
	case <-time.After(90 * time.Second):
		t.Fatal("harness: waitForReady neither returned nor failed within 90 s")
	}
	elapsed := time.Since(start)

	rec.mu.Lock()
	msg, failed := rec.msg, rec.failed
	rec.mu.Unlock()
	if !failed {
		t.Fatal("harness: waitForReady reported ready a server whose Start failed")
	}
	if !strings.Contains(msg, "unknown engine type") || elapsed > 5*time.Second {
		t.Fatalf("celeris#706: waitForReady failed after %v with %q; want Start's own error "+
			"(unknown engine type), reported as soon as Start returns", elapsed.Round(time.Millisecond), msg)
	}
	// The error is still there for the caller's shutdown.
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "unknown engine type") {
			t.Fatalf("done holds %v after waitForReady, want Start's error", err)
		}
	default:
		t.Fatal("waitForReady consumed Start's error; the caller's shutdown would block on done")
	}
}
