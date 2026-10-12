package eventloop

// celeris#887 item 18, portable worker: a reader goroutine whose onRecv calls
// runtime.Goexit ends without returning, and the conn is never read again and
// never reports onClose. It must be logged, as on the Linux workers (see
// worker_exit_887_linux_test.go).

import (
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAPortableReaderWhoseOnRecvGoexitsIsReported887(t *testing.T) {
	h := c887Capture(t)
	l := newPortableTestLoop(t, 1)
	srv := newLoopbackServer(t)
	client, server := srv.dial(t)
	if _, err := registrar(t, l, 0).RegisterNetConn(client, func([]byte) { runtime.Goexit() }, func(error) {}); err != nil {
		t.Fatalf("RegisterNetConn: %v", err)
	}
	if _, err := server.Write([]byte("x")); err != nil {
		t.Fatalf("server write: %v", err)
	}
	select {
	case <-h.got:
	case <-time.After(5 * time.Second):
		t.Fatal("a portable reader goroutine ended in a runtime.Goexit and nothing was logged")
	}
	recs := h.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(recs), recs)
	}
	r := recs[0]
	var stack string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "stack" {
			stack = a.Value.String()
		}
		return true
	})
	if r.Level != slog.LevelError || !strings.Contains(r.Message, "portable") {
		t.Errorf("record %v %q, want an Error that names the portable worker", r.Level, r.Message)
	}
	if !strings.Contains(stack, "Goexit") {
		t.Errorf("the stack does not show the Goexit call:\n%s", stack)
	}
	// Loop.Close must not hang on the reader that is gone.
	done := make(chan struct{})
	go func() { _ = l.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Loop.Close did not return within 10 s after a reader goroutine ended in a Goexit")
	}
}

func TestACloseOfAHealthyPortableLoopReportsNothing887(t *testing.T) {
	h := c887Capture(t)
	l := newPortableTestLoop(t, 1)
	srv := newLoopbackServer(t)
	client, server := srv.dial(t)
	rec := newRecorder()
	if _, err := registrar(t, l, 0).RegisterNetConn(client, rec.onRecv, rec.onClose); err != nil {
		t.Fatalf("RegisterNetConn: %v", err)
	}
	if _, err := server.Write([]byte("x")); err != nil {
		t.Fatalf("server write: %v", err)
	}
	rec.waitFor(t, 1)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = rec.waitClose(t)
	if recs := h.snapshot(); len(recs) != 0 {
		t.Errorf("a normal Close logged %d record(s): %v", len(recs), recs)
	}
}
