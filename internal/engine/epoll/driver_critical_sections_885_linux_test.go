//go:build linux

package epoll

// celeris#885 items 3 and 23: the two critical sections of the driver paths
// that celeris#710 and celeris#771 rely on, each pinned by a nil-by-default
// seam inside it (testHookDriverReadChecked, testHookCloseDriverDeleted)
// instead of by where a test hook happens to sit relative to the code.

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// bareDriverLoop885 is a Loop with an epoll instance of its own and nothing
// else: no engine, no worker goroutine, no driver conn registered. A test
// drives driverRead and closeDriver on its own goroutine, so the interleaving
// is the test's, and RegisterConn's ADD lands in an instance nobody waits on.
// The epoll descriptor is real: a zero epollFD would be stdin.
func bareDriverLoop885(t *testing.T) *Loop {
	t.Helper()
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Fatalf("epoll_create1: %v", err)
	}
	l := &Loop{epollFD: epfd, listenFD: -1, timerFD: -1}
	t.Cleanup(l.closeEpollFD)
	return l
}

// TestDriverReadCheckAndReadAreOneCriticalSection885 is the epoll port of the
// standalone loop's TestReadCheckAndReadAreOneCriticalSection784 (#843 did the
// standalone loop only): driverRead's closed check and its read(2) must be one
// critical section with the flag UnregisterConn sets before it returns. A
// reader that checks and then reads in a separate step passes the #710 tests
// (driver_unregister_readloop_test.go), which unregister while the reader is
// parked in onRecv, before its next check; #772's surviving mutant m1-toctou is
// such a reader. It fails here: testHookDriverReadChecked runs between the
// check and the read, and from inside it the test starts UnregisterConn(A) on
// another goroutine and gives it 300 ms.
//
// If UnregisterConn returns in that time, the reader has been overtaken
// between its check and its read: the test closes A, moves X onto A's number
// with bytes waiting, and lets the read go, which then reads X. If it does not
// return, UnregisterConn is waiting for the read, as it must; the test does the
// close and the reuse once it has returned, and then offers the conn a read
// turn for the old number, which must read nothing.
func TestDriverReadCheckAndReadAreOneCriticalSection885(t *testing.T) {
	l := bareDriverLoop885(t)
	a, aPeer := socketpairNonblocking(t)
	t.Cleanup(func() { _ = unix.Close(aPeer) })
	if _, err := unix.Write(aPeer, []byte("AAAA")); err != nil {
		t.Fatalf("queue A's bytes: %v", err)
	}

	const stolen = "XXXXXXXXXX"
	var (
		armed     = true
		hookRan   bool
		overtaken bool
		aClosed   bool
		x0, x1    = -1, -1
		unregDone = make(chan error, 1)
		chunks    [][]byte
		closes    int
	)
	// reuse is what the caller does once UnregisterConn(A) has returned: close
	// A's descriptor, and put a new socket X, with bytes waiting, on its number.
	reuse := func() {
		if err := unix.Close(a); err != nil {
			t.Fatalf("close A: %v", err)
		}
		aClosed = true
		x0, x1 = takeNumber710(t, a, true)
		if _, err := unix.Write(x1, []byte(stolen)); err != nil {
			t.Fatalf("queue X's bytes: %v", err)
		}
	}
	t.Cleanup(func() {
		if x0 >= 0 {
			closeX710(a, x0, x1)
		} else if !aClosed {
			_ = unix.Close(a)
		}
	})
	// This test starts no worker, so no other goroutine reads the var.
	t.Cleanup(func() { testHookDriverReadChecked = nil })
	testHookDriverReadChecked = func(dc *driverConn) {
		if dc.fd != a || !armed {
			return
		}
		armed = false
		hookRan = true
		go func() { unregDone <- l.UnregisterConn(a) }()
		select {
		case err := <-unregDone:
			if err != nil {
				t.Errorf("UnregisterConn(A): %v", err)
			}
			overtaken = true
			reuse()
		case <-time.After(300 * time.Millisecond):
		}
	}

	if err := l.RegisterConn(a, func(b []byte) {
		chunks = append(chunks, append([]byte(nil), b...))
	}, func(error) { closes++ }); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}
	dc := l.lookupDriver(a)
	if dc == nil {
		t.Fatal("A is not registered")
	}

	l.driverRead(dc)
	if !hookRan {
		t.Fatal("testHookDriverReadChecked never ran for A: driverRead did not go through the hooked read")
	}
	if !overtaken {
		select {
		case err := <-unregDone:
			if err != nil {
				t.Fatalf("UnregisterConn(A): %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("UnregisterConn(A) did not return within 5 s of the read")
		}
		reuse()
		// A turn the loop still owed A when it was unregistered (the queue holds
		// conns, not numbers): the conn is gone, so it must read nothing.
		if more := l.driverRead(dc); more {
			t.Error("driverRead of an unregistered conn reports bytes left")
		}
	}

	got, rerr := readAvailable(a)
	stole := false
	for _, c := range chunks {
		if bytes.Contains(c, []byte(stolen)) {
			stole = true
		}
	}
	t.Logf("885 critical-section: UnregisterConn returned while the reader was between its check and its read: %v; "+
		"onRecv got %d chunk(s), X's bytes among them: %v; onClose ran %d time(s); X (fd %d) reads %q (err %v)",
		overtaken, len(chunks), stole, closes, a, got, rerr)
	if overtaken {
		t.Errorf("UnregisterConn(A) returned while the reader was between its closed check and its read: the two are not one critical section")
	}
	if stole {
		t.Errorf("A's onRecv got X's bytes: the reader read A's number after UnregisterConn(A) returned and X took it")
	}
	if got != stolen {
		t.Errorf("X, which took A's number, reads %q (err %v), want %q: the reader consumed X's bytes", got, rerr, stolen)
	}
	if len(chunks) != 1 || string(chunks[0]) != "AAAA" {
		t.Errorf("A's onRecv got %q, want exactly A's own \"AAAA\" (the read UnregisterConn waited for is still delivered)", chunks)
	}
	if closes != 1 {
		t.Errorf("A's onClose ran %d times, want 1", closes)
	}
}

// TestCloseDriverDeletesInsideItsClaimSection885: closeDriver removes the
// descriptor from the interest set in the critical section that claims the
// conn closed, not after it. celeris#771: UnregisterConn takes dc.mu before it
// returns, so while closeDriver holds dc.mu from the claim to the DEL, the
// caller can neither return nor close the number and register another conn on
// it. TestCloseDriverSparesConnOnReusedNumber771 pins where the DEL sits
// relative to its own hook (testHookCloseDriverClaimed, which runs with no
// lock held), which a DEL moved to just after dc.mu.Unlock() still satisfies;
// that DEL has #771's second window, and it passes every 771 and 775 test. This
// one runs inside the section (testHookCloseDriverDeleted) and asserts three
// things there: dc.mu is held, the conn is claimed closed, and the descriptor
// is out of the interest set, which EPOLL_CTL_MOD answers with ENOENT.
//
// The same MOD, made before closeDriver, must succeed: the ENOENT afterwards is
// the DEL's, not a wrong epoll descriptor's or a wrong event's.
func TestCloseDriverDeletesInsideItsClaimSection885(t *testing.T) {
	l := bareDriverLoop885(t)
	a, aPeer := socketpairNonblocking(t)
	t.Cleanup(func() { _ = unix.Close(a); _ = unix.Close(aPeer) })
	closes := 0
	if err := l.RegisterConn(a, func([]byte) {}, func(error) { closes++ }); err != nil {
		t.Fatalf("RegisterConn(A): %v", err)
	}
	dc := l.lookupDriver(a)
	if dc == nil {
		t.Fatal("A is not registered")
	}

	mod := func() error {
		ev := dc.epollEvent(unix.EPOLLIN | unix.EPOLLET | unix.EPOLLRDHUP)
		return l.driverEpollCtl(unix.EPOLL_CTL_MOD, dc.fd, &ev)
	}
	if err := mod(); err != nil {
		t.Fatalf("PREMISE: EPOLL_CTL_MOD of the registered descriptor: %v (the ENOENT below would prove nothing)", err)
	}

	var (
		ran, held, claimed bool
		modErr             error
	)
	testHookCloseDriverDeleted = func(got *driverConn) {
		ran = true
		if got != dc {
			t.Errorf("the hook was given another conn")
		}
		if held = !dc.mu.TryLock(); !held {
			dc.mu.Unlock()
		}
		claimed = dc.closed
		modErr = mod()
	}
	t.Cleanup(func() { testHookCloseDriverDeleted = nil })

	l.closeDriver(dc, nil)

	if !ran {
		t.Fatal("testHookCloseDriverDeleted never ran: closeDriver did not reach its DEL")
	}
	if !held {
		t.Error("dc.mu was not held at the DEL: the DEL is outside the critical section that claims the conn closed")
	}
	if !claimed {
		t.Error("the conn was not claimed closed at the DEL")
	}
	if !errors.Is(modErr, unix.ENOENT) {
		t.Errorf("EPOLL_CTL_MOD in the claim section after the DEL: %v, want ENOENT: the descriptor was still in the "+
			"interest set, so an UnregisterConn(A) that returns before the DEL lets the caller close the number "+
			"and register another conn the DEL then removes (celeris#771)", modErr)
	}
	if closes != 1 {
		t.Errorf("onClose ran %d times, want 1", closes)
	}
	if l.lookupDriver(a) != nil {
		t.Error("the conn is still in the map after closeDriver")
	}
}
