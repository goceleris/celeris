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
// listener. Every byte must reach A's onRecv without a ResumeAccept.
//
// Since celeris#885 item 39 the loop does not park at all while A is
// registered (a driver conn it registers is a conn it serves, and one
// registered after the park would be stranded: see the probes below), so the
// loop must be awake once the last byte is in, and park once A is gone. The
// first version of this test asserted the park with A still registered.
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
	// A is still registered: the loop stays out of the park for a while after
	// the last byte (long enough for several turns of a 500 ms wait).
	parkedWhileRegistered := false
	for dl := time.Now().Add(1200 * time.Millisecond); !parkedWhileRegistered && time.Now().Before(dl); time.Sleep(time.Millisecond) {
		parkedWhileRegistered = loop.suspended.Load()
	}
	// And once A is gone nothing holds it: it parks.
	_ = wl.UnregisterConn(rd)
	parked := false
	for dl := time.Now().Add(3 * time.Second); !parked && time.Now().Before(dl); time.Sleep(time.Millisecond) {
		parked = loop.suspended.Load()
	}
	t.Logf("C930 paused loop: %d of %d bytes delivered without a ResumeAccept; parked while A was registered: %v; parked after A was unregistered: %v",
		got, size, parkedWhileRegistered, parked)
	if !delivered {
		t.Errorf("A got %d of its %d bytes: the loop parked with them still owed", got, size)
	}
	if parkedWhileRegistered {
		t.Errorf("the loop parked while a driver conn was registered on it (celeris#885 item 39)")
	}
	if !parked {
		t.Errorf("the loop did not park within 3 s of the last driver conn leaving")
	}
}

// awaitSuspended930 waits for the loop to park (suspended is set under wakeMu
// by the park, and cleared by wakeIfSuspended).
func awaitSuspended930(t *testing.T, loop *Loop, within time.Duration) bool {
	t.Helper()
	for dl := time.Now().Add(within); time.Now().Before(dl); time.Sleep(time.Millisecond) {
		if loop.suspended.Load() {
			return true
		}
	}
	return loop.suspended.Load()
}

// awaitBytes930 waits for the sink to have received want bytes.
func awaitBytes930(s *c930Sink, within time.Duration) bool {
	select {
	case <-s.all:
		return true
	case <-time.After(within):
		return false
	}
}

// TestAPausedLoopServesADriverConnRegisteredBeforeThePark885x39: after
// PauseAccept on an engine with no HTTP conn, a byte written to the peer of a
// driver conn registered on the loop must reach its onRecv without a
// ResumeAccept. On the base the loop parked on connCount == 0 (it counts HTTP
// conns only) and left epoll_wait for good, 3 of 3 processes (DFAIR lane's
// probe, #885 item 39).
func TestAPausedLoopServesADriverConnRegisteredBeforeThePark885x39(t *testing.T) {
	old := deferlinger.SetLinger(0)
	t.Cleanup(func() { deferlinger.SetLinger(old) })
	e := c930HTTPEngine(t)
	wl := e.WorkerLoop(0)
	loop := wl.(*Loop)
	a, peer := c930Socketpair(t)
	t.Cleanup(func() { _ = unix.Close(a); _ = unix.Close(peer) })
	const size = 64
	s := c930NewSink(0, size)
	if err := wl.RegisterConn(a, s.onRecv, s.onClose); err != nil {
		t.Fatalf("RegisterConn: %v", err)
	}
	t.Cleanup(func() { _ = wl.UnregisterConn(a) })
	if err := e.PauseAccept(); err != nil {
		t.Fatalf("PauseAccept: %v", err)
	}
	// Give a loop that would park the time to do it (it parks the iteration
	// after its listener closes); the fixed loop never does.
	parkedBefore := awaitSuspended930(t, loop, time.Second)
	if n, err := unix.Write(peer, c930Pattern(0, size)); n != size || err != nil {
		t.Fatalf("write to the peer: %d of %d (%v)", n, size, err)
	}
	delivered := awaitBytes930(s, 3*time.Second)
	t.Logf("885x39 registered before the park: parked before the write: %v; delivered within 3 s: %v; parked at the end: %v",
		parkedBefore, delivered, loop.suspended.Load())
	if !delivered {
		t.Errorf("the byte written to a driver conn on a paused loop never reached onRecv within 3 s (parked before the write: %v)", parkedBefore)
	}
	if parkedBefore {
		t.Errorf("the loop parked while a driver conn was registered on it")
	}
}

// TestADriverConnRegisteredAfterThePauseParkedTheLoopIsServed885x39: the loop
// is parked (no HTTP conn, no driver conn, listener closed), and then a
// driver conn is registered on it. RegisterConn must end the park: the
// descriptor is in the epoll set, but a parked loop is not in epoll_wait. Two
// arms: the bytes are in the socket before the registration (the ADD reports
// the first edge itself), and they arrive after it.
func TestADriverConnRegisteredAfterThePauseParkedTheLoopIsServed885x39(t *testing.T) {
	for _, tc := range []struct {
		name        string
		writeBefore bool
	}{
		{"bytes-before-the-registration", true},
		{"bytes-after-the-registration", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := deferlinger.SetLinger(0)
			t.Cleanup(func() { deferlinger.SetLinger(old) })
			e := c930HTTPEngine(t)
			wl := e.WorkerLoop(0)
			loop := wl.(*Loop)
			if err := e.PauseAccept(); err != nil {
				t.Fatalf("PauseAccept: %v", err)
			}
			if !awaitSuspended930(t, loop, 5*time.Second) {
				t.Fatal("premise: the idle paused loop did not park")
			}
			a, peer := c930Socketpair(t)
			t.Cleanup(func() { _ = unix.Close(a); _ = unix.Close(peer) })
			const size = 64
			s := c930NewSink(0, size)
			write := func() {
				if n, err := unix.Write(peer, c930Pattern(0, size)); n != size || err != nil {
					t.Fatalf("write to the peer: %d of %d (%v)", n, size, err)
				}
			}
			if tc.writeBefore {
				write()
			}
			if err := wl.RegisterConn(a, s.onRecv, s.onClose); err != nil {
				t.Fatalf("RegisterConn: %v", err)
			}
			t.Cleanup(func() { _ = wl.UnregisterConn(a) })
			if !tc.writeBefore {
				write()
			}
			delivered := awaitBytes930(s, 3*time.Second)
			t.Logf("885x39 registered after the park (%s): delivered within 3 s: %v; parked at the end: %v",
				tc.name, delivered, loop.suspended.Load())
			if !delivered {
				t.Errorf("a driver conn registered on a parked loop never got its bytes within 3 s: RegisterConn left the loop parked")
			}
			if loop.suspended.Load() {
				t.Errorf("the loop is still parked with a driver conn registered on it")
			}
		})
	}
}
