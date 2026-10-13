//go:build unix

package memcached

// celeris#890 item 48 (from #935 review r1): every event loop finds a driver
// conn by its descriptor NUMBER, so Close must unregister the number while it
// still names the conn's own socket, and only then close the file. The
// end-to-end guards of celeris#859 (close_after_teardown_859_linux_test.go)
// catch an onClose that releases the number; they do not catch a Close that
// closes the file before it calls UnregisterConn (the review's arm x9: the
// whole memcached suite passed), where UnregisterConn would act on whatever conn
// has taken the number.
//
// These tests build a memcachedConn by hand on a loop that records, for every call
// by number, which socket the number named at that moment (by inode), as
// driver/postgres/close_after_teardown_859_test.go does. They need no event
// loop and no server, so they run on every unix platform, macOS included.

import (
	"io"
	"os"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// order890Loop is an engine.WorkerLoop that keeps nothing and records every
// call by number.
type order890Loop struct {
	mu    sync.Mutex
	calls []order890Call
}

type order890Call struct {
	op  string
	fd  int
	ino uint64 // inode the number named at the call; 0 when it named nothing
}

func (l *order890Loop) record(op string, fd int) {
	l.mu.Lock()
	l.calls = append(l.calls, order890Call{op: op, fd: fd, ino: ino890(fd)})
	l.mu.Unlock()
}

func (l *order890Loop) RegisterConn(int, func([]byte), func(error)) error { return nil }
func (l *order890Loop) UnregisterConn(fd int) error                       { l.record("UnregisterConn", fd); return nil }
func (l *order890Loop) Write(fd int, _ []byte) error                      { l.record("Write", fd); return nil }
func (l *order890Loop) CPUID() int                                        { return -1 }

func (l *order890Loop) snapshot() []order890Call {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]order890Call(nil), l.calls...)
}

// ino890 returns the inode of the file fd names, or 0 if fd names nothing.
func ino890(fd int) uint64 {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0
	}
	return st.Ino
}

func open890(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return err == nil
}

// new890Conn returns a loop-mode memcachedConn on one end of a socket pair, the
// recording loop, and the inode of the conn's socket.
func new890Conn(t *testing.T) (c *memcachedConn, loop *order890Loop, ino uint64) {
	t.Helper()
	p, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(p[1]) })
	loop = &order890Loop{}
	c = &memcachedConn{
		fd:    p[0],
		file:  os.NewFile(uintptr(p[0]), "memcached-890"),
		loop:  loop,
		state: newMCState(false),
	}
	ino = ino890(p[0])
	if ino == 0 {
		t.Fatalf("fixture: the conn's socket (number %d) has no inode", p[0])
	}
	return c, loop, ino
}

// checkClose890 fails t unless the loop saw exactly one UnregisterConn, by the
// conn's number, while the number still named the conn's own socket, and the
// number is closed afterwards.
func checkClose890(t *testing.T, loop *order890Loop, n int, ino uint64) {
	t.Helper()
	unreg := 0
	for _, call := range loop.snapshot() {
		if call.op != "UnregisterConn" {
			continue
		}
		unreg++
		if call.fd != n {
			t.Errorf("UnregisterConn(%d): want the conn's number %d", call.fd, n)
		}
		if call.ino != ino {
			t.Errorf("UnregisterConn(%d) reached inode %d, not the conn's socket (inode %d): Close had released the number before it unregistered", n, call.ino, ino)
		}
	}
	if unreg != 1 {
		t.Errorf("Close made %d UnregisterConn calls, want exactly 1", unreg)
	}
	if open890(n) {
		t.Errorf("number %d is still open after Close: the conn's socket was not closed", n)
	}
}

// Close on a live conn unregisters the number while it is still the conn's
// own, then closes the file.
func TestMemcachedCloseUnregistersBeforeItReleasesTheNumber890(t *testing.T) {
	c, loop, ino := new890Conn(t)
	n := c.fd
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	checkClose890(t, loop, n, ino)
}

// After a loop teardown (onClose), which must keep the number, Close still
// unregisters it on the conn's own socket.
func TestMemcachedCloseAfterOnCloseUnregistersOnItsOwnSocket890(t *testing.T) {
	c, loop, ino := new890Conn(t)
	n := c.fd
	c.onClose(io.EOF)
	if !open890(n) || ino890(n) != ino {
		t.Fatalf("onClose released the number %d (inode now %d, want %d)", n, ino890(n), ino)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	checkClose890(t, loop, n, ino)
}
