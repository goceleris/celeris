//go:build linux

package epoll

// celeris#885 (a neighbour of item 18, in the helper's sites): the MOD that
// resumes a detached conn's reads (drainDetachQueue) restored EPOLLIN|EPOLLET
// and dropped EPOLLRDHUP, which the conn's ADD and every EPOLLOUT arm and
// disarm carry. A conn that went through one pause and resume stopped being
// told of a peer half-close.

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRecvResumeKeepsRDHUPInterest885(t *testing.T) {
	l := newLedgerLoop(t)
	l.async = true
	fd, peer, cs := adoptedPair885(t, l)
	drainEvents885(l)

	cs.recvPauseDesired.Store(true)
	l.enqueueDetach(cs)
	l.drainDetachQueue()
	cs.recvPauseDesired.Store(false)
	l.enqueueDetach(cs)
	l.drainDetachQueue()
	if cs.recvPaused {
		t.Fatal("the resume was not applied")
	}
	// The byte the peer wrote is still unread: the resume MOD reports it.
	drainEvents885(l)

	// The peer half-closes: a conn registered for EPOLLRDHUP is told so.
	if err := unix.Shutdown(peer, unix.SHUT_WR); err != nil {
		t.Fatalf("shutdown the peer: %v", err)
	}
	ev, ok := eventFor885(t, l, fd, 2*time.Second)
	if !ok {
		t.Fatal("no event after the peer's half-close")
	}
	if ev.Events&unix.EPOLLRDHUP == 0 {
		t.Errorf("after a pause and a resume the conn's events are %#x: the resume's EPOLL_CTL_MOD dropped EPOLLRDHUP, so a peer half-close is no longer reported as one", ev.Events)
	}
}
