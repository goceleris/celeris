package websocket

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
// test that owns it. Everything else goes to the embedded TB.
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

func (f *fatalRecorder) result() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.msg, f.failed
}

// TestWaitForReadyFailsFastOnStartError pins waitForReady's handling of a
// Start that returns before the server is ready (celeris#706), on every
// platform: it fails at once with Start's error, and leaves that error in done
// for the caller's shutdown, which receives from done as well
// (TestEngineIntegration defers `<-done` before it waits). The engine type is
// one no engine factory knows, so Start fails at once with "unknown engine
// type".
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

	msg, failed := rec.result()
	if !failed {
		t.Fatal("harness: waitForReady reported ready a server whose Start failed")
	}
	if !strings.Contains(msg, "unknown engine type") || elapsed > 5*time.Second {
		t.Fatalf("celeris#706: waitForReady failed after %v with %q; want Start's own error "+
			"(unknown engine type), reported as soon as Start returns", elapsed.Round(time.Millisecond), msg)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "unknown engine type") {
			t.Fatalf("done holds %v after waitForReady, want Start's error", err)
		}
	default:
		t.Fatal("waitForReady consumed Start's error: a caller's shutdown receiving from done would block for good")
	}
}
