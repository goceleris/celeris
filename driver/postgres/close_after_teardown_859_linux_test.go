//go:build linux

package postgres

// celeris#859, end to end on the standalone driver loop: the server hangs up,
// the loop tears conn A down and fires A's onClose, and only then does the
// pool Close A (what database/sql does with a bad conn). A's Close writes
// Terminate and unregisters by number. If A's number has been released by
// then, the next socket the process opens takes it (B here, registered on
// A's worker), and A's Close sends B's server A's Terminate and tears B down.

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/driver/internal/eventloop"
)

func TestPgCloseAfterTeardownLeavesTheNumbersNextConnAlone859(t *testing.T) {
	hangup := make(chan struct{})
	addr := startFakePG(t, func(c net.Conn) {
		fakePGTrustStartup(t, c, 42, 1, func(net.Conn) { <-hangup })
	})
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	host, port, _ := net.SplitHostPort(addr)
	dsn := DSN{
		Host: host, Port: port, User: "u", Database: "d",
		Options: Options{SSLMode: "disable", StatementCacheSize: 16},
		Params:  map[string]string{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialConn(ctx, prov, nil, dsn, 0)
	if err != nil {
		t.Fatalf("dialConn: %v", err)
	}
	n := c.fd
	aIno := c859Ino(n)
	if aIno == 0 {
		t.Fatalf("fixture: A's socket (number %d) has no inode", n)
	}

	// The server hangs up; the loop tears A down and fires A's onClose.
	close(hangup)
	deadline := time.Now().Add(5 * time.Second)
	for !c.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !c.closed.Load() {
		t.Fatal("A was not torn down within 5 s of the server's hangup")
	}

	// B is the next socket the process opens. If A's number is free, the
	// kernel gives it to B (dup2 makes that certain); if A still holds it,
	// B gets a number of its own.
	bOnA := !c859Open(n)
	var b, bPeer int
	if bOnA {
		b, bPeer = c859Socket(t, n)
		t.Errorf("A's onClose released number %d before A's Close; B takes it", n)
	} else if got := c859Ino(n); got != aIno {
		t.Fatalf("A's number %d names another socket (inode %d, A's is %d) after A's teardown: A released it and the process has reused it", n, got, aIno)
	} else {
		b, bPeer = c859Socket(t, -1)
	}
	if got := c859Ino(b); got == 0 || got == aIno {
		t.Fatalf("fixture: B (number %d) has inode %d, want nonzero and not A's %d", b, got, aIno)
	}
	_ = unix.SetNonblock(b, true)
	_ = unix.SetNonblock(bPeer, true)
	wl := prov.WorkerLoop(0) // A's worker
	bClosed := make(chan error, 1)
	if err := wl.RegisterConn(b, func([]byte) {}, func(err error) { bClosed <- err }); err != nil {
		t.Fatalf("RegisterConn(B, number %d) on A's worker: %v", b, err)
	}
	t.Cleanup(func() { _ = wl.UnregisterConn(b); _ = unix.Close(b); _ = unix.Close(bPeer) })

	// Control: B works before A's Close.
	if err := wl.Write(b, []byte("pre")); err != nil {
		t.Fatalf("control: Write(B) before A's Close: %v", err)
	}
	var pre [16]byte
	pk, _ := unix.Read(bPeer, pre[:])
	if pk <= 0 || string(pre[:pk]) != "pre" {
		t.Fatalf("control: B's peer read %q before A's Close, want \"pre\"", pre[:max(pk, 0)])
	}

	// The pool closes the bad conn A.
	_ = c.Close()

	var buf [64]byte
	k, _ := unix.Read(bPeer, buf[:])
	var bOnClose bool
	select {
	case <-bClosed:
		bOnClose = true
	default:
	}
	werr := wl.Write(b, []byte("hi"))
	t.Logf("859: A number %d, B number %d (on A's number: %v); B's peer got %q from A's Close; B's onClose fired: %v; Write(B) after A's Close: %v",
		n, b, bOnA, buf[:max(k, 0)], bOnClose, werr)
	if k > 0 {
		t.Errorf("A's Close wrote %q to B", buf[:k])
	}
	if bOnClose || werr != nil {
		t.Errorf("A's Close unregistered B (onClose fired %v, Write err %v)", bOnClose, werr)
	}
	if !bOnA && c859Ino(n) == aIno {
		t.Errorf("number %d still names A's socket after A's Close: Close did not release it", n)
	}
}
