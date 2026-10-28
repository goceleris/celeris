//go:build linux

package iouring

import (
	"testing"
	"time"
)

// TestRefusedCloseIsNotRescuedByAStragglerAck889: the idle clock's cap refused a
// conn (the peer is not taking its output) and closeRefused set its drain clock so
// that the sweep after next reaps it. ACKs for bytes that were in flight at the
// moment of the decision still arrive after it; the drain's own look at the peer's
// acknowledged bytes (noteClosingAcked) reads them as the peer taking bytes, gives
// the conn another drain bound (up to WriteTimeout), and the conn stays open.
// closeRefused makes no such look available to the sweep that follows (the zero-
// window cell failed once on a CI runner: the conn was still open 9.5 s after).
func TestRefusedCloseIsNotRescuedByAStragglerAck889(t *testing.T) {
	s := newStall888(t, 30*time.Second) // the drain bound is max(5 s, WriteTimeout): 30 s
	fillSocket888(t, s.cs.fd)
	fd := s.cs.fd
	s.w.closeRefused(fd, s.cs, time.Now().UnixNano())
	if !s.cs.closing {
		t.Fatal("closeRefused did not defer the close behind the SEND in flight")
	}
	// A straggler: the peer takes a few KiB after the decision, and its
	// acknowledgements reach the server.
	buf := make([]byte, 8192)
	_ = s.peer.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, _ := s.peer.Read(buf); n == 0 {
		t.Fatal("the peer read nothing: the socket was not full")
	}
	time.Sleep(100 * time.Millisecond)
	// The reap needs a ring, which this rig has not: reaching it panics on the
	// nil ring, and that is the proof. A sweep that gave the conn another bound
	// returns normally (its lastActivity restamped).
	reaped := func() (r bool) {
		defer func() { r = recover() != nil }()
		s.w.checkTimeouts()
		return false
	}()
	if !reaped {
		t.Fatalf("the refused conn was not reaped by the sweep after it: a straggler's ACK gave it another drain bound (lastActivity %v ago)", time.Since(time.Unix(0, s.cs.lastActivity)).Round(time.Millisecond))
	}
}
