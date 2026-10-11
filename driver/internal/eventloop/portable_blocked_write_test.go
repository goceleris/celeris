package eventloop

// A Write blocked because the peer stops reading must be released by
// Loop.Close and by UnregisterConn, and an onClose that writes must not hang:
// Loop.Close reports ErrLoopClosed without holding a lock across the callback
// (RULE 10 check of the order in portableConn.shutdownConn).

import (
	"errors"
	"net"
	"testing"
	"time"
)

func blockedWriter(t *testing.T, l *Loop) (id int, rec *recorder, werr chan error) {
	t.Helper()
	srv := newLoopbackServer(t)
	client, _ := srv.dial(t) // the server side never reads
	if tc, ok := client.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4 << 10)
	}
	rec = newRecorder()
	id, err := registrar(t, l, 0).RegisterNetConn(client, rec.onRecv, rec.onClose)
	if err != nil {
		t.Fatalf("RegisterNetConn: %v", err)
	}
	werr = make(chan error, 1)
	big := make([]byte, 64<<20)
	go func() { werr <- l.WorkerLoop(0).Write(id, big) }()
	select {
	case err := <-werr:
		t.Fatalf("64 MiB Write to a peer that never reads returned early: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	return id, rec, werr
}

func TestPortableLoopCloseReleasesABlockedWrite(t *testing.T) {
	l := newPortableTestLoop(t, 1)
	_, rec, werr := blockedWriter(t, l)
	done := make(chan error, 1)
	go func() { done <- l.Close() }()
	select {
	case <-done:
	case <-time.After(portableWait):
		t.Fatal("Loop.Close hung behind a blocked Write")
	}
	select {
	case err := <-werr:
		if err == nil {
			t.Fatal("blocked Write returned nil after Loop.Close")
		}
	case <-time.After(portableWait):
		t.Fatal("blocked Write not released by Loop.Close")
	}
	if err := rec.waitClose(t); !errors.Is(err, ErrLoopClosed) {
		t.Fatalf("onClose(%v), want ErrLoopClosed", err)
	}
}

func TestPortableUnregisterReleasesABlockedWrite(t *testing.T) {
	l := newPortableTestLoop(t, 1)
	id, _, werr := blockedWriter(t, l)
	if err := l.WorkerLoop(0).UnregisterConn(id); err != nil {
		t.Fatalf("UnregisterConn: %v", err)
	}
	select {
	case err := <-werr:
		if err == nil {
			t.Fatal("blocked Write returned nil after UnregisterConn")
		}
	case <-time.After(portableWait):
		t.Fatal("blocked Write not released by UnregisterConn")
	}
}

// onClose writes to its own conn while Loop.Close runs it: it must not hang.
func TestPortableOnCloseWritingDuringLoopCloseDoesNotHang(t *testing.T) {
	srv := newLoopbackServer(t)
	l := newPortableTestLoop(t, 1)
	client, _ := srv.dial(t)
	var id int
	fired := make(chan error, 1)
	id, err := registrar(t, l, 0).RegisterNetConn(client, nil, func(e error) {
		fired <- l.WorkerLoop(0).Write(id, make([]byte, 64<<20))
	})
	if err != nil {
		t.Fatalf("RegisterNetConn: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = l.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(portableWait):
		t.Fatal("Loop.Close hung on an onClose that writes")
	}
	if err := <-fired; !errors.Is(err, ErrLoopClosed) {
		t.Fatalf("Write from onClose during Loop.Close: %v, want ErrLoopClosed", err)
	}
}
