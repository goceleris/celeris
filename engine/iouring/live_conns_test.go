//go:build linux

package iouring

import "testing"

func TestLiveConnsAddRemove(t *testing.T) {
	w := &Worker{conns: make([]*connState, 1024), liveConns: make([]int, 0, 16)}

	w.addLiveConn(5)
	w.addLiveConn(10)
	w.addLiveConn(15)
	if got := len(w.liveConns); got != 3 {
		t.Fatalf("after 3 adds: len(liveConns) = %d, want 3", got)
	}

	// Remove middle — swap-with-last should preserve order: [5, 15]
	w.removeLiveConn(10)
	if got := len(w.liveConns); got != 2 {
		t.Fatalf("after 1 remove: len(liveConns) = %d, want 2", got)
	}
	if w.liveConns[0] != 5 || w.liveConns[1] != 15 {
		t.Errorf("liveConns = %v, want [5 15]", w.liveConns)
	}

	// Remove last
	w.removeLiveConn(15)
	if got := len(w.liveConns); got != 1 {
		t.Fatalf("after 1 remove: len(liveConns) = %d, want 1", got)
	}
	if w.liveConns[0] != 5 {
		t.Errorf("liveConns[0] = %d, want 5", w.liveConns[0])
	}

	// Remove remaining
	w.removeLiveConn(5)
	if got := len(w.liveConns); got != 0 {
		t.Errorf("after final remove: len(liveConns) = %d, want 0", got)
	}

	// Remove non-existent is a no-op (not an error)
	w.removeLiveConn(999)
}

func TestLiveConnsHighVolume(t *testing.T) {
	// Sanity: 16k add/remove cycles should be O(1) each and produce
	// the same final state.
	w := &Worker{conns: make([]*connState, 65536), liveConns: make([]int, 0, 16384)}

	for fd := 0; fd < 16384; fd++ {
		w.addLiveConn(fd)
	}
	if got := len(w.liveConns); got != 16384 {
		t.Fatalf("after 16k adds: len = %d, want 16384", got)
	}

	// Remove all
	for fd := 0; fd < 16384; fd++ {
		w.removeLiveConn(fd)
	}
	if got := len(w.liveConns); got != 0 {
		t.Errorf("after 16k removes: len = %d, want 0", got)
	}
}
