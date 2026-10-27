//go:build linux

package epoll

import (
	"bufio"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestPoolWaitGateRequestInTheWindowBeforeCloseIsCut1104 measures what is left
// of the race after the fix, and shows it is the one any close has. The pass
// asks the kernel's receive queue for a conn (TIOCINQ), then runs the send-side
// tests, then closeConn (shutdown(SHUT_WR), then close(2)): a request that
// reaches the kernel in between meets a conn judged idle. The hook
// (testHookGateBeforeClose) sends one in that window, to one conn of the pass:
// the client sees its conn closed with the request unanswered at once (EOF, the
// FIN closeConn's SHUT_WR sends ahead of the RST that close(2) sends for the
// unread bytes; or ECONNRESET), not a hang; every other conn is unaffected (a
// FIN, as for any idle conn). The window is the few microseconds of the
// send-side tests. Closing it altogether would take a lingering close (keep the
// conn, read and discard until the peer's FIN), which answers nothing either: the
// request is lost in both, and an HTTP client retries it on either (a keep-alive
// conn that fails before any byte of a response came back; std's Shutdown has
// the same window when it closes an idle conn). Not done: see the PR's findings.
func TestPoolWaitGateRequestInTheWindowBeforeCloseIsCut1104(t *testing.T) {
	h := &gateUnread1104Handler{started: make(chan struct{}, 1), release: make(chan struct{})}
	h.pass = gatePass1104
	e, addr := startE2E885(t, false, h, auto1104)
	h.eng.Store(e)
	type client struct {
		c  net.Conn
		br *bufio.Reader
	}
	byAddr := map[string]client{}
	for i := 0; i < 16; i++ {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		defer func() { _ = c.Close() }()
		cl := client{c, bufio.NewReader(c)}
		if _, err := get1104(cl.c, cl.br, "/x"); err != nil {
			t.Fatalf("first request %d: %v", i, err)
		}
		byAddr[c.LocalAddr().String()] = cl
	}
	b, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	self := b.LocalAddr().String()
	h.self.Store(&self)
	var hit atomic.Pointer[string]
	hook := func(fd int) {
		if hit.Load() != nil {
			return
		}
		l := h.held.Load()
		if l == nil || fd >= len(l.conns) || l.conns[fd] == nil {
			return
		}
		cs := l.conns[fd]
		a := cs.remoteAddr
		cl, ok := byAddr[a]
		if !ok {
			return
		}
		hit.Store(&a)
		if _, err := cl.c.Write([]byte("GET /late HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			t.Errorf("write in the window: %v", err)
			return
		}
		// Wait until the request is in the kernel's receive queue.
		for dl := time.Now().Add(2 * time.Second); time.Now().Before(dl); {
			if q, err := unix.IoctlGetInt(fd, unix.TIOCINQ); err == nil && q > 0 {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Errorf("the request did not reach the receive queue")
	}
	testHookGateBeforeClose.Store(&hook)
	defer testHookGateBeforeClose.Store(nil)
	if _, err := b.Write([]byte("GET /gate HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the /gate handler did not start")
	}
	close(h.release)
	time.Sleep(100 * time.Millisecond)

	a := hit.Load()
	if a == nil {
		t.Fatal("the hook found no conn to send into (the held loop closed none)")
	}
	reset, fin, other := 0, 0, 0
	for ca, cl := range byAddr {
		_ = cl.c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		_, err := cl.br.ReadByte()
		switch {
		case ca == *a:
			t.Logf("the conn with a request in the window read: %v", err)
			if !errors.Is(err, syscall.ECONNRESET) && err != io.EOF {
				t.Errorf("the conn that had a request in the window read %v, want EOF or ECONNRESET", err)
			}
			if errors.Is(err, syscall.ECONNRESET) {
				reset++
			}
		case err == io.EOF:
			fin++
		case isTimeout1104(err):
			other++
		default:
			t.Errorf("conn %s read %v, want a FIN or nothing", ca, err)
		}
	}
	t.Logf("the conn with a request in the window was closed unanswered (reset=%d, else EOF); the others: %d FIN, %d left alone (the other loop's)", reset, fin, other)
}
