//go:build linux

package redis

// celeris#859, family check: postgres's onClose closed the conn's fd, so its
// later Close wrote and unregistered by a number another conn had taken.
// Redis keeps the number until Close: after a loop teardown the number
// still names the conn's own socket, and Close leaves the next conn alone.

import (
	"bufio"
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/driver/internal/eventloop"
)

func c859Ino(fd int) uint64 {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0
	}
	return st.Ino
}

func TestRedisCloseAfterTeardownLeavesTheNumbersNextConnAlone859(t *testing.T) {
	fake := startFakeRedis(t, func(cmd []string, w *bufio.Writer) {
		if len(cmd) > 0 && strings.EqualFold(cmd[0], "HELLO") {
			handleHELLO(w, 3)
			return
		}
		writeSimple(w, "OK")
	})
	prov, err := eventloop.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer eventloop.Release(prov)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialRedisConn(ctx, prov, Config{Addr: fake.Addr()}, 0)
	if err != nil {
		t.Fatalf("dialRedisConn: %v", err)
	}
	n := c.fd
	aIno := c859Ino(n)
	if aIno == 0 {
		t.Fatalf("fixture: A's socket (number %d) has no inode", n)
	}

	// The server hangs up; the loop tears A down and fires A's onClose. The
	// dial can return before the server has accepted, so wait for that.
	deadline := time.Now().Add(5 * time.Second)
	for {
		fake.mu.Lock()
		accepted := len(fake.conns)
		if accepted > 0 {
			for _, sc := range fake.conns {
				_ = sc.Close()
			}
		}
		fake.mu.Unlock()
		if accepted > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the server did not accept A within 5 s")
		}
		time.Sleep(time.Millisecond)
	}
	deadline = time.Now().Add(5 * time.Second)
	for !c.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !c.closed.Load() {
		t.Fatal("A was not torn down within 5 s of the server's hangup")
	}

	// B is the next socket the process opens: on A's number if it is free.
	p, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, bPeer := p[0], p[1]
	bOnA := false
	if _, err := unix.FcntlInt(uintptr(n), unix.F_GETFD, 0); err != nil {
		switch n {
		case b:
		case bPeer: // a lower number was free: n is the pair's second end
			b, bPeer = bPeer, b
		default:
			if err := unix.Dup2(b, n); err != nil {
				t.Fatal(err)
			}
			_ = unix.Close(b)
			b = n
		}
		bOnA = true
		t.Errorf("A's teardown released number %d before A's Close; B takes it", n)
	} else if got := c859Ino(n); got != aIno {
		t.Fatalf("A's number %d names another socket (inode %d, A's is %d) after A's teardown: A released it and the process has reused it", n, got, aIno)
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
