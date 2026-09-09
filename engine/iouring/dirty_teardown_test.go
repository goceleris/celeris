//go:build linux

package iouring

import (
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

// newDirtyTestWorker builds the minimum Worker a teardown path touches:
// the conn table plus the engine-wide counters it decrements.
func newDirtyTestWorker(fds ...int) *Worker {
	n := 8
	for _, fd := range fds {
		if fd+1 > n {
			n = fd + 1
		}
	}
	return &Worker{
		conns:       make([]*connState, n),
		activeConns: new(atomic.Int64),
		closeCount:  new(atomic.Uint64),
	}
}

// socketPairFDs returns a connected pair of real descriptors. Tests here must
// never invent fd numbers: the teardown paths close cs.fd.
func socketPairFDs(t *testing.T) (int, int) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("socketpair: %v", err)
	}
	return fds[0], fds[1]
}

// dirtyList walks the worker's intrusive dirty list and returns the entries in
// order. It stops at a generous bound so a corrupted (self-referential) list
// fails the test instead of hanging it.
func dirtyList(t *testing.T, w *Worker) []*connState {
	t.Helper()
	var out []*connState
	for cs := w.dirtyHead; cs != nil; cs = cs.dirtyNext {
		out = append(out, cs)
		if len(out) > 1000 {
			t.Fatal("dirty list did not terminate — cycle or corruption")
		}
	}
	return out
}

// TestFinishCloseUnlinksFromDirtyList pins celeris#527 for the two close
// paths. A connState torn down while still on the dirty list used to stay
// linked: the loop's only removeDirty is inside "if !cs.sending", so a conn
// closed with a SEND in flight is skipped by the very code that would unlink
// it. For a detached conn that is unbounded — nothing ever clears cs.sending
// once w.conns[fd] is nil, because the cancelled SEND's CQE is discarded as
// stale — and adaptiveTimeout returns 0 while dirtyHead is non-nil, so the
// worker busy-spins at 100% CPU even when idle.
func TestFinishCloseUnlinksFromDirtyList(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sending  bool
		detached bool
	}{
		{"plain", false, false},
		{"sending", true, false},
		{"detached", false, true},
		{"detached_sending", true, true}, // the immortal-entry case
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Real descriptors: the close paths under test call unix.Close
			// on cs.fd, so a made-up small integer would close whatever the
			// test process has there — fd 2 is stderr, and killing it takes
			// the whole binary down with SIGPIPE on the next write.
			fd, other := socketPairFDs(t)
			keepFD, keepOther := socketPairFDs(t)
			defer unix.Close(other)
			defer unix.Close(keepOther)
			defer unix.Close(keepFD)

			w := newDirtyTestWorker(fd, keepFD)
			keep := &connState{fd: keepFD, liveIdx: -1}
			cs := &connState{fd: fd, liveIdx: -1, sending: tc.sending}
			w.conns[keepFD], w.conns[fd] = keep, cs
			w.connCount = 2
			w.markDirty(keep)
			w.markDirty(cs)
			if got := len(dirtyList(t, w)); got != 2 {
				t.Fatalf("setup: dirty list has %d entries, want 2", got)
			}

			// A conn with a SEND in flight reaches cancelConnOps, which
			// needs a live ring this unit Worker does not have. The unlink
			// is deliberately sequenced BEFORE that, so recovering here
			// still pins exactly what this test is about — and asserts the
			// ordering: an unlink placed after the cancel would be skipped.
			func() {
				defer func() {
					if r := recover(); r != nil && !tc.sending {
						panic(r)
					}
				}()
				if tc.detached {
					w.finishCloseDetached(fd, cs)
				} else {
					w.finishClose(fd)
				}
			}()

			for _, e := range dirtyList(t, w) {
				if e == cs {
					t.Fatal("torn-down connState is still linked in the dirty list")
				}
			}
			if cs.dirty || cs.dirtyNext != nil || cs.dirtyPrev != nil {
				t.Errorf("dirty links not cleared: dirty=%v next=%v prev=%v",
					cs.dirty, cs.dirtyNext != nil, cs.dirtyPrev != nil)
			}
			// The unrelated connection must survive: unlinking one entry
			// must not truncate the list behind it.
			if got := dirtyList(t, w); len(got) != 1 || got[0] != keep {
				t.Errorf("unlinking truncated the list: %d entries remain, want just the other conn", len(got))
			}
		})
	}
}

// TestFinishCloseUnlinksDirtyHead covers the worst variant of celeris#527: the
// torn-down entry is the list HEAD. Left linked and then pooled, w.dirtyHead
// points at a recycled connState; when the pool hands it to a new connection,
// markDirty computes dirtyNext = dirtyHead = itself and builds a
// self-referential node, and the dirty loop then spins forever.
func TestFinishCloseUnlinksDirtyHead(t *testing.T) {
	fd, other := socketPairFDs(t)
	defer unix.Close(other)
	w := newDirtyTestWorker(fd)
	head := &connState{fd: fd, liveIdx: -1}
	w.conns[fd] = head
	w.connCount = 1
	w.markDirty(head)
	if w.dirtyHead != head {
		t.Fatal("setup: expected the conn to be the dirty head")
	}

	w.finishClose(fd)

	if w.dirtyHead != nil {
		t.Fatal("dirtyHead still points at the torn-down connState")
	}
	// Recycle it the way connStatePool would, then re-dirty it. Without the
	// unlink this is where the self-reference forms.
	reused := head
	reused.fd = other
	if other >= len(w.conns) {
		w.conns = append(w.conns, make([]*connState, other+1-len(w.conns))...)
	}
	w.conns[other] = reused
	w.markDirty(reused)
	if reused.dirtyNext == reused || reused.dirtyPrev == reused {
		t.Fatal("self-referential dirty node: the loop would spin forever")
	}
	if got := dirtyList(t, w); len(got) != 1 || got[0] != reused {
		t.Errorf("re-dirtied conn not linked cleanly: %d entries", len(got))
	}
}

// TestHijackConnUnlinksFromDirtyList pins the hijack variant. It is not only a
// leak there: the fd stays open under the caller's net.Conn, and the dirty
// loop's retry pass calls prepareRecv on what it walks — re-arming exactly the
// recv hijackConn cancels to stop it stealing the hijacker's first bytes.
func TestHijackConnUnlinksFromDirtyList(t *testing.T) {
	// A real socketpair: hijackConn hands the fd to os.NewFile/net.FileConn,
	// which takes ownership and closes it. Handing it an arbitrary integer
	// would close whatever descriptor the test process happens to have at
	// that number.
	fd, other := socketPairFDs(t)
	defer unix.Close(other)

	w := newDirtyTestWorker(fd)
	cs := &connState{fd: fd, liveIdx: -1}
	w.conns[fd] = cs
	w.connCount = 1
	w.markDirty(cs)

	c, err := w.hijackConn(fd)
	if err != nil {
		t.Fatalf("hijackConn: %v", err)
	}
	defer c.Close()

	if cs.dirty || w.dirtyHead == cs {
		t.Fatal("hijacked connState is still linked in the dirty list")
	}
}
