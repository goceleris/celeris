//go:build !linux

package eventloop

// The real Loop on a platform that uses the portable worker (darwin,
// windows): New builds it, and its workers adopt a net.Conn directly.

import (
	"errors"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
)

func TestLoopWorkersAdoptNetConns(t *testing.T) {
	srv := newLoopbackServer(t)
	l, err := New(2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if l.NumWorkers() != 2 {
		t.Fatalf("NumWorkers = %d, want 2", l.NumWorkers())
	}

	for n := 0; n < l.NumWorkers(); n++ {
		reg := registrar(t, l, n)
		w := l.WorkerLoop(n)
		client, server := srv.dial(t)
		rec := newRecorder()
		id, err := reg.RegisterNetConn(client, rec.onRecv, rec.onClose)
		if err != nil {
			t.Fatalf("worker %d: RegisterNetConn: %v", n, err)
		}
		if _, err := server.Write([]byte("hello")); err != nil {
			t.Fatalf("server write: %v", err)
		}
		if got := rec.waitFor(t, 5); string(got) != "hello" {
			t.Fatalf("worker %d: onRecv delivered %q", n, got)
		}
		if err := w.Write(id, []byte("world")); err != nil {
			t.Fatalf("worker %d: Write: %v", n, err)
		}
		if got := readN(t, server, 5); string(got) != "world" {
			t.Fatalf("worker %d: server read %q", n, got)
		}
		if err := w.UnregisterConn(id); err != nil {
			t.Fatalf("worker %d: UnregisterConn: %v", n, err)
		}
		if err := rec.waitClose(t); err != nil {
			t.Fatalf("worker %d: onClose(%v), want nil", n, err)
		}
		if err := w.UnregisterConn(id); !errors.Is(err, engine.ErrUnknownFD) {
			t.Fatalf("worker %d: second UnregisterConn: %v", n, err)
		}
	}
}
