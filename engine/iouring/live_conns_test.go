//go:build linux

package iouring

import (
	"math/rand"
	"testing"
)

// regLive registers a connState at fd in w.conns and adds it to liveConns,
// mirroring what onAcceptedFD does for the bookkeeping under test.
func regLive(w *Worker, fd int) *connState {
	cs := &connState{fd: fd, liveIdx: -1}
	w.conns[fd] = cs
	w.addLiveConn(cs)
	return cs
}

func TestLiveConnsAddRemove(t *testing.T) {
	w := &Worker{conns: make([]*connState, 1024), liveConns: make([]int, 0, 16)}

	cs5 := regLive(w, 5)
	cs10 := regLive(w, 10)
	cs15 := regLive(w, 15)
	if got := len(w.liveConns); got != 3 {
		t.Fatalf("after 3 adds: len(liveConns) = %d, want 3", got)
	}
	// liveIdx stamps must reflect insertion order.
	if cs5.liveIdx != 0 || cs10.liveIdx != 1 || cs15.liveIdx != 2 {
		t.Fatalf("liveIdx stamps wrong: %d %d %d, want 0 1 2", cs5.liveIdx, cs10.liveIdx, cs15.liveIdx)
	}

	// Remove middle — swap-with-last moves 15 into slot 1: [5, 15].
	w.removeLiveConn(cs10)
	if got := len(w.liveConns); got != 2 {
		t.Fatalf("after 1 remove: len(liveConns) = %d, want 2", got)
	}
	if w.liveConns[0] != 5 || w.liveConns[1] != 15 {
		t.Errorf("liveConns = %v, want [5 15]", w.liveConns)
	}
	// The swapped-in element (15) must have its liveIdx updated to 1.
	if cs15.liveIdx != 1 {
		t.Errorf("swapped-in cs15.liveIdx = %d, want 1", cs15.liveIdx)
	}
	if cs10.liveIdx != -1 {
		t.Errorf("removed cs10.liveIdx = %d, want -1", cs10.liveIdx)
	}

	// Remove last
	w.removeLiveConn(cs15)
	if got := len(w.liveConns); got != 1 {
		t.Fatalf("after 1 remove: len(liveConns) = %d, want 1", got)
	}
	if w.liveConns[0] != 5 {
		t.Errorf("liveConns[0] = %d, want 5", w.liveConns[0])
	}

	// Remove remaining
	w.removeLiveConn(cs5)
	if got := len(w.liveConns); got != 0 {
		t.Errorf("after final remove: len(liveConns) = %d, want 0", got)
	}

	// Double-remove (stale liveIdx == -1) is a no-op.
	w.removeLiveConn(cs5)
	// Removing a nil cs is a no-op.
	w.removeLiveConn(nil)
}

// TestLiveConnsHighVolumeReverse removes in REVERSE order, exercising the
// swap-with-last path on every removal (the original forward-order test
// happened to remove the head each time and never tripped the swap).
func TestLiveConnsHighVolumeReverse(t *testing.T) {
	const n = 16384
	w := &Worker{conns: make([]*connState, 65536), liveConns: make([]int, 0, n)}

	css := make([]*connState, n)
	for fd := 0; fd < n; fd++ {
		css[fd] = regLive(w, fd)
	}
	if got := len(w.liveConns); got != n {
		t.Fatalf("after %d adds: len = %d, want %d", n, got, n)
	}

	for fd := n - 1; fd >= 0; fd-- {
		w.removeLiveConn(css[fd])
	}
	if got := len(w.liveConns); got != 0 {
		t.Errorf("after reverse removes: len = %d, want 0", got)
	}
}

// TestLiveConnsHighVolumeShuffled removes in a shuffled order and verifies the
// liveIdx bookkeeping stays self-consistent throughout (every remaining
// element's liveIdx points back at its own fd).
func TestLiveConnsHighVolumeShuffled(t *testing.T) {
	const n = 8192
	w := &Worker{conns: make([]*connState, 65536), liveConns: make([]int, 0, n)}

	css := make([]*connState, n)
	for fd := 0; fd < n; fd++ {
		css[fd] = regLive(w, fd)
	}

	order := rand.Perm(n)
	for _, fd := range order {
		w.removeLiveConn(css[fd])
		// Invariant: every live entry's liveIdx maps back to itself.
		for i, lfd := range w.liveConns {
			if w.conns[lfd].liveIdx != i {
				t.Fatalf("liveIdx desync at slot %d: fd=%d has liveIdx=%d", i, lfd, w.conns[lfd].liveIdx)
			}
		}
		w.conns[fd] = nil
	}
	if got := len(w.liveConns); got != 0 {
		t.Errorf("after shuffled removes: len = %d, want 0", got)
	}
}

func BenchmarkRemoveLiveConn(b *testing.B) {
	// Each iteration: add a conn, remove the HEAD (worst case for an O(N)
	// scan, O(1) for the index-based remove).
	const n = 4096
	w := &Worker{conns: make([]*connState, 65536), liveConns: make([]int, 0, n)}
	css := make([]*connState, n)
	for fd := 0; fd < n; fd++ {
		css[fd] = regLive(w, fd)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Remove head then re-add it to keep the population stable.
		head := w.liveConns[0]
		cs := w.conns[head]
		w.removeLiveConn(cs)
		w.addLiveConn(cs)
	}
}
