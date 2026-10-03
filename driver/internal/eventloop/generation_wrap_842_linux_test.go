//go:build linux

package eventloop

// celeris#842, review round 2 of #933: the per-worker registration
// generation wraps past 0.

import (
	"math"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestGenerationsWrapPastZero842: the worker's registration generation wraps
// from MaxUint32 to 1, never to 0, and the conns registered on either side of
// the wrap are each served. 0 names no registration (worker.gen).
func TestGenerationsWrapPastZero842(t *testing.T) {
	l, err := New(1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	w := l.WorkerLoop(0).(*worker)
	w.mu.Lock()
	w.gen = math.MaxUint32 - 1
	w.mu.Unlock()

	type conn struct {
		fd, peer int
		got      chan []byte
	}
	var cs []conn
	for i := range 3 {
		fd, peer := socketPair(t)
		t.Cleanup(func() { _ = unix.Close(fd); _ = unix.Close(peer) })
		c := conn{fd, peer, make(chan []byte, 4)}
		if err := w.RegisterConn(fd, func(b []byte) { c.got <- append([]byte(nil), b...) }, func(error) {}); err != nil {
			t.Fatalf("RegisterConn %d: %v", i, err)
		}
		t.Cleanup(func() { _ = w.UnregisterConn(fd) })
		cs = append(cs, c)
	}
	var gens []uint32
	for _, c := range cs {
		gens = append(gens, c784Lookup(w, c.fd).gen)
	}
	t.Logf("C842 wrap: generations %v", gens)
	if gens[0] != math.MaxUint32 || gens[1] != 1 || gens[2] != 2 {
		t.Errorf("generations %v across the wrap, want [%d 1 2]", gens, uint32(math.MaxUint32))
	}
	for i, c := range cs {
		c784Queue(t, c.peer, []byte{'a' + byte(i)})
		select {
		case b := <-c.got:
			if len(b) != 1 || b[0] != 'a'+byte(i) {
				t.Errorf("conn %d (generation %d) got %q, want %q", i, gens[i], b, []byte{'a' + byte(i)})
			}
		case <-time.After(2 * time.Second):
			t.Errorf("conn %d (generation %d) was not served within 2 s", i, gens[i])
		}
	}
}
