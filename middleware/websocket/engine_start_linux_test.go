//go:build linux

package websocket

import (
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestStartNativeServerFailsFastOnStartError is the failing-first oracle for
// celeris#706. startNativeServerWithHandle runs Start in a goroutine that
// sends Start's error on a channel, and then polled only s.Addr() and a dial,
// for 30 s, never reading that channel. A server whose Start failed therefore
// cost the full 30 s and failed with "server not ready within timeout", which
// reads the same as a server that hung and drops the cause: an io_uring_setup
// ENOMEM at the CI shape's 8 MiB memlock, a refused bind.
//
// The engine type is one no engine factory knows, so Start fails at once with
// "unknown engine type" on every kernel and runner. The helper runs against a
// recorder in place of the test's TB; the recorder's Fatal ends the helper's
// goroutine as t.Fatal would.
//
// The oracle: the helper fails with Start's own error, and at once.
func TestStartNativeServerFailsFastOnStartError(t *testing.T) {
	const unknownEngine = celeris.EngineType(250)
	rec := &fatalRecorder{TB: t}
	start := time.Now()
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_, shutdown, _ := startNativeServerWithHandle(rec, unknownEngine, Config{Handler: func(*Conn) {}})
		shutdown() // reached only if the helper reported the server ready
	}()
	select {
	case <-exited:
	case <-time.After(90 * time.Second):
		t.Fatal("harness: the helper neither returned nor failed within 90 s")
	}
	elapsed := time.Since(start)

	msg, failed := rec.result()
	if !failed {
		t.Fatal("harness: the helper reported ready a server whose Start failed")
	}
	if !strings.Contains(msg, "unknown engine type") || elapsed > 5*time.Second {
		t.Fatalf("celeris#706: the helper failed after %v with %q; want Start's own error "+
			"(unknown engine type), reported as soon as Start returns",
			elapsed.Round(time.Millisecond), msg)
	}
	t.Logf("the helper failed after %v with %q", elapsed.Round(time.Millisecond), msg)
}
