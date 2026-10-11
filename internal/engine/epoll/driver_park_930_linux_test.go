//go:build linux

package epoll

// celeris#930, review round 3: a loop that PauseAccept parks and the read
// turns a driver conn is owed. On the base one event drains the socket before
// the loop looks at its park condition, so every byte in the socket when the
// loop parks has been delivered. With a read budget the loop can owe a conn
// turns at that point, and a loop that parks anyway strands what they would
// have read until the next ResumeAccept.

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/deferlinger"
)

// TestAPausedLoopServesTheBacklogItOwesBeforeItParks930: A's source is a pipe
// sized to 1 MiB and filled before A is registered, and A's onRecv takes 3 ms a
// chunk, so the backlog takes the loop 32 chunks (24 ms a turn, about 100 ms in
// all). The engine is paused (linger 0) right after: the loop closes its
// listener and has nothing else to hold it, so it parks as soon as its park
// condition holds. Every byte must reach A's onRecv without a ResumeAccept, and
// the loop must park once the last has.
func TestAPausedLoopServesTheBacklogItOwesBeforeItParks930(t *testing.T) {
	old := deferlinger.SetLinger(0)
	t.Cleanup(func() { deferlinger.SetLinger(old) })
	e := c930HTTPEngine(t)
	wl := e.WorkerLoop(0)
	loop := wl.(*Loop)
	rd, wr := c930Pipe(t)
	t.Cleanup(func() { _ = unix.Close(rd); _ = unix.Close(wr) })
	// 100 bytes short of 32 reads: the last read is short, so the loop does not read
	// on to an EOF (the write end stays open).
	const size = 1<<20 - 100
	if n, err := unix.Write(wr, c930Pattern(0, size)); n != size || err != nil {
		t.Fatalf("fill the pipe: wrote %d of %d (%v)", n, size, err)
	}
	s := c930NewSink(0, size)
	if err := wl.RegisterConn(rd, func(b []byte) {
		s.onRecv(b)
		time.Sleep(3 * time.Millisecond)
	}, s.onClose); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	t.Cleanup(func() { _ = wl.UnregisterConn(rd) })
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	delivered := false
	select {
	case <-s.all:
		delivered = true
	case <-time.After(5 * time.Second):
	}
	s.mu.Lock()
	got := s.got
	s.mu.Unlock()
	parked := false
	for dl := time.Now().Add(3 * time.Second); !parked && time.Now().Before(dl); time.Sleep(time.Millisecond) {
		parked = loop.suspended.Load()
	}
	t.Logf("C930 paused loop: %d of %d bytes delivered without a ResumeAccept; the loop parked afterwards: %v", got, size, parked)
	if !delivered {
		t.Errorf("A got %d of its %d bytes: the loop parked with them still owed", got, size)
	}
	if !parked {
		t.Errorf("the loop did not park within 3 s of the last byte")
	}
}
