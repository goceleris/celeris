//go:build linux

package epoll

// celeris#930, review round 3: a write error on a driver conn that is owed a
// read turn. On the base one event reads the socket to its end before it
// serves EPOLLOUT, so a write error closes the conn after every byte that was
// in the socket has been delivered. handleDriverEvent serves EPOLLOUT at once,
// and for a conn that is queued or stopped at its budget the bytes left in the
// socket would come after the close. The close has to wait for the turn that
// drains them, as the teardown flags do.

import (
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// c930Fill writes the counter pattern (byte i is i%251) to fd until the socket
// takes no more, and returns the number of bytes it took.
func c930Fill(t *testing.T, fd int) int {
	t.Helper()
	pat := c930Pattern(0, 4<<20)
	off := 0
	for off < len(pat) {
		n, err := unix.Write(fd, pat[off:min(off+64<<10, len(pat))])
		if n > 0 {
			off += n
		}
		if err == unix.EAGAIN {
			return off
		}
		if err != nil {
			t.Fatalf("fill: %v", err)
		}
	}
	t.Fatal("fill: the socket took 4 MiB: the test cannot load a turn")
	return 0
}

// TestAWriteErrorOnAConnOwedBytesClosesItAfterThem930: A's own send buffer is
// full, A's peer has put more than one turn's budget of bytes into A's
// socket, A's owner has a write pending with EPOLLOUT armed, and then the
// flush fails. onClose must fire once, with an error, after every byte the
// peer sent.
//
//   - theTurnStopsAtItsBudget: the peer closes while the loop is held, so the
//     event is A's first and carries EPOLLOUT with EPOLLHUP/EPOLLERR. The turn
//     reads its budget, stops with bytes left, and then serves EPOLLOUT.
//   - theEventComesWhileQueued: the same close, but it reaches the loop while A
//     is queued with bytes left (a hook closes the peer in the epoll_wait before
//     A's next turn), and the merge of the event serves EPOLLOUT at once.
//
// The second needs the hook the fix adds (testHookDriverQueueWait), which the
// base does not call: only the first says anything about origin/main.
func TestAWriteErrorOnAConnOwedBytesClosesItAfterThem930(t *testing.T) {
	for _, tc := range []struct {
		name   string
		queued bool // the event comes while A is queued (a hook), else A's first event
	}{
		{"theTurnStopsAtItsBudget", false},
		{"theEventComesWhileQueued", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hook atomic.Pointer[func()]
			var fired atomic.Bool
			testHookDriverQueueWait = func(int, int) {
				if h := hook.Load(); h != nil && fired.CompareAndSwap(false, true) {
					(*h)()
				}
			}
			t.Cleanup(func() { testHookDriverQueueWait = nil }) // runs last, after the engine is stopped
			eng, stop := newTestEngine(t)
			t.Cleanup(stop)
			wl := eng.WorkerLoop(0)

			a, peer := socketpairNonblocking(t)
			peerOpen := true
			closePeer := func() {
				if peerOpen {
					peerOpen = false
					_ = unix.Close(peer)
				}
			}
			t.Cleanup(func() { closePeer(); _ = unix.Close(a) })
			_ = c930Fill(t, a) // A's send buffer is full: A's first write hits EAGAIN
			// The socket takes about 230 KB with the default send buffer (a turn reads
			// 256 KiB); an explicit SO_SNDBUF is doubled by the kernel, up to twice
			// net.core.wmem_max.
			if err := unix.SetsockoptInt(peer, unix.SOL_SOCKET, unix.SO_SNDBUF, 1<<20); err != nil {
				t.Fatalf("SO_SNDBUF: %v", err)
			}
			fed := c930Fill(t, peer)
			// The first 100 bytes are read here, so that the rest is not a multiple of
			// the read buffer: the last read of the drain is short, and the conn is
			// closed by the write error and not by a zero-byte read (see
			// TestBudgetedDriverReadsDeliverEveryByteInOrder930).
			const skip = 100
			if n, err := unix.Read(a, make([]byte, skip)); n != skip || err != nil {
				t.Fatalf("read the first %d bytes: %d, %v", skip, n, err)
			}
			want := fed - skip
			if want <= driverReadBudget*driverReadBufSize+4096 {
				t.Fatalf("the peer's %d bytes fit one turn (%d reads of %d): the test cannot stop a turn at its budget", want, driverReadBudget, driverReadBufSize)
			}
			s := c930NewSink(skip, 0) // want 0: s.all is not used
			armWrite := func() {
				if err := wl.Write(a, make([]byte, 1<<20)); err != nil {
					t.Errorf("Write: %v", err)
				}
			}
			var release chan struct{}
			if !tc.queued {
				// The loop is held in P's onRecv while A's state is prepared, so
				// that A's first event carries everything.
				p, pPeer := socketpairNonblocking(t)
				t.Cleanup(func() { _ = unix.Close(p); _ = unix.Close(pPeer) })
				entered := make(chan struct{})
				release = make(chan struct{})
				var once atomic.Bool
				if err := wl.RegisterConn(p, func([]byte) {
					if once.CompareAndSwap(false, true) {
						close(entered)
						<-release
					}
				}, func(error) {}); err != nil {
					t.Fatalf("RegisterConn(P): %v", err)
				}
				t.Cleanup(func() { _ = wl.UnregisterConn(p) })
				if _, err := unix.Write(pPeer, []byte{'p'}); err != nil {
					t.Fatalf("write P: %v", err)
				}
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("the loop never ran P's onRecv")
				}
				t.Cleanup(func() {
					select {
					case <-release:
					default:
						close(release)
					}
				})
			} else {
				h := func() { // on the loop's goroutine, A queued, before the epoll_wait that collects the event
					armWrite()
					closePeer()
				}
				hook.Store(&h)
			}
			if err := wl.RegisterConn(a, s.onRecv, s.onClose); err != nil {
				t.Fatalf("RegisterConn(A): %v", err)
			}
			t.Cleanup(func() { _ = wl.UnregisterConn(a) })
			if !tc.queued {
				armWrite()
				dc := wl.(*Loop).lookupDriver(a)
				armed := false
				for dl := time.Now().Add(2 * time.Second); !armed && time.Now().Before(dl); time.Sleep(time.Millisecond) {
					dc.mu.Lock()
					armed = dc.epollOut
					dc.mu.Unlock()
				}
				if !armed {
					t.Fatal("EPOLLOUT was not armed after a write into a full socket")
				}
				closePeer()
				close(release)
			}
			var closeErr error
			select {
			case closeErr = <-s.closed:
			case <-time.After(10 * time.Second):
				s.mu.Lock()
				got := s.got
				s.mu.Unlock()
				if tc.queued && !fired.Load() {
					t.Fatalf("the epoll_wait hook never ran (%d of %d bytes delivered): the test did not drive the window", got, want)
				}
				t.Fatalf("onClose never fired (%d of %d bytes delivered)", got, want)
			}
			s.mu.Lock()
			got, atClose, closes, bad := s.got, s.atClose, s.closes, s.bad
			s.mu.Unlock()
			t.Logf("C930 write error: the peer put %d bytes into A; %d delivered at onClose (%d calls, error: %v), first byte out of order at %d", want, atClose, closes, closeErr, bad)
			if atClose != want || got != want || bad >= 0 {
				t.Errorf("onClose fired with %d of %d bytes delivered (%d at the end, first byte out of order: %d): the write error closed the conn before its owed bytes", atClose, want, got, bad)
			}
			if closes != 1 || closeErr == nil {
				t.Errorf("onClose fired %d times, with error %v: want once, with the error that closed the conn", closes, closeErr)
			}
		})
	}
}
