//go:build linux

package epoll

// celeris#930, review round 3: the deferred close of a failed flush, driven from
// the test goroutine (see driver_write_error_930_linux_test.go for the same
// defect with real events and the helpers).

import (
	"testing"

	"golang.org/x/sys/unix"
)

// TestAFlushErrorOfAConnOwedBytesClosesItAfterThemWhateverTheEventCarries930
// drives the loop's functions from the test goroutine, as the loop thread, with
// events it writes itself: a flush that fails in an event that carries no
// EPOLLHUP/EPOLLERR/EPOLLRDHUP (the real events of a peer that closed carry
// them, and close the conn after the drain whatever the flush does) must
// still defer the close to the turn that drains the socket, with the write
// error as its cause. A's socket holds more than a turn's budget and its peer
// is gone, so every flush fails with EPIPE.
//
//   - theConnIsQueued: A is in the queue when the event comes (the merge path).
//   - theTurnStopsAtItsBudget: the event is A's first and the turn reads its
//     budget and stops with bytes left.
func TestAFlushErrorOfAConnOwedBytesClosesItAfterThemWhateverTheEventCarries930(t *testing.T) {
	for _, queued := range []bool{true, false} {
		name := "theTurnStopsAtItsBudget"
		if queued {
			name = "theConnIsQueued"
		}
		t.Run(name, func(t *testing.T) {
			epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
			if err != nil {
				t.Fatalf("epoll_create1: %v", err)
			}
			t.Cleanup(func() { _ = unix.Close(epfd) })
			l := &Loop{epollFD: epfd, listenFD: -1, timerFD: -1}
			a, peer := socketpairNonblocking(t)
			t.Cleanup(func() { _ = unix.Close(a) })
			_ = c930Fill(t, a) // A's send buffer is full
			if err := unix.SetsockoptInt(peer, unix.SOL_SOCKET, unix.SO_SNDBUF, 1<<20); err != nil {
				t.Fatalf("SO_SNDBUF: %v", err)
			}
			fed := c930Fill(t, peer)
			const skip = 100 // see TestAWriteErrorOnAConnOwedBytesClosesItAfterThem930
			if n, err := unix.Read(a, make([]byte, skip)); n != skip || err != nil {
				t.Fatalf("read the first %d bytes: %d, %v", skip, n, err)
			}
			want := fed - skip
			if want <= driverReadBudget*driverReadBufSize+4096 {
				t.Fatalf("the peer's %d bytes fit one turn (%d reads of %d)", want, driverReadBudget, driverReadBufSize)
			}
			s := c930NewSink(skip, 0)
			if err := l.RegisterConn(a, s.onRecv, s.onClose); err != nil {
				t.Fatalf("RegisterConn: %v", err)
			}
			dc := l.lookupDriver(a)
			if err := l.Write(a, make([]byte, 1<<20)); err != nil { // EAGAIN: queued in the conn, EPOLLOUT armed
				t.Fatalf("Write: %v", err)
			}
			if !dc.epollOut {
				t.Fatal("EPOLLOUT was not armed after a write into a full socket")
			}
			_ = unix.Close(peer) // from here every flush fails
			if queued {
				l.queueDriverRead(dc)
			}
			l.handleDriverEvent(dc, unix.EPOLLIN|unix.EPOLLOUT) // no EPOLLHUP/EPOLLERR/EPOLLRDHUP
			s.mu.Lock()
			closesAfterEvent, gotAfterEvent := s.closes, s.got
			s.mu.Unlock()
			if closesAfterEvent != 0 {
				t.Fatalf("onClose fired in the event itself, with %d of %d bytes delivered: the write error closed the conn before its owed bytes", gotAfterEvent, want)
			}
			if !dc.readQueued || dc.readErr == nil {
				t.Fatalf("after the event the conn is queued: %v, with the write error recorded: %v", dc.readQueued, dc.readErr != nil)
			}
			turns := 0
			for ; turns < 100; turns++ {
				s.mu.Lock()
				done := s.closes != 0
				s.mu.Unlock()
				if done {
					break
				}
				l.serveDriverReadQ(len(l.driverReadQ))
			}
			var closeErr error
			select {
			case closeErr = <-s.closed:
			default:
			}
			s.mu.Lock()
			got, atClose, closes, bad := s.got, s.atClose, s.closes, s.bad
			s.mu.Unlock()
			t.Logf("C930 flush error: %d bytes owed; %d delivered at onClose after %d more turns (%d calls, error: %v), first byte out of order at %d", want, atClose, turns, closes, closeErr, bad)
			if closes != 1 || atClose != want || got != want || bad >= 0 {
				t.Errorf("onClose fired %d times, with %d of %d bytes delivered (%d at the end, first byte out of order: %d)", closes, atClose, want, got, bad)
			}
			if closeErr == nil {
				t.Errorf("onClose fired with a nil error: the write error that closed the conn was lost")
			}
		})
	}
}
