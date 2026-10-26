//go:build linux

package epoll

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// celeris#886 item 25: a hand-off that the target refuses puts the conn back on
// the loop it came from (reclaimTransplant), with the bytes it had carried: the
// start of a pipelined next request that drainRead had read, and counted as
// the recv that read them, before tryTransplant moved the conn. The re-adoption
// replays them as the recv event of a fresh adoption, and counted one more: the
// same bytes counted twice by the loop that never gave them up.
func TestReclaimTransplantDoesNotCountTheCarriedRecvTwice886(t *testing.T) {
	l := newHandoffLoop(t, nil)
	l.handler = newPathHandler885(16)
	target := &refusingTarget{}
	l.transplant.Store(&transplantState{target: target})
	fd, peer, cs := adoptedPair885(t, l)
	var b [8]byte
	_, _ = unix.Read(fd, b[:]) // the byte adoptedPair885 wrote to have the ADD report EPOLLIN

	// One whole request, then the first bytes of the next: the parser is at a
	// request boundary with those bytes buffered, which is what a sync source
	// carries.
	if _, err := unix.Write(peer, []byte("GET /a HTTP/1.1\r\nHost: x\r\n\r\nGET /b HT")); err != nil {
		t.Fatalf("peer write: %v", err)
	}
	l.drainRead(fd, time.Now().UnixNano())
	if l.reqBatch != 1 {
		t.Fatalf("apparatus: drainRead counted %d recvs, want 1", l.reqBatch)
	}
	if !cs.h1State.AtRequestBoundary() || !cs.h1State.HasPendingData() {
		t.Fatalf("apparatus: the conn is not at a boundary with the next request's first bytes buffered")
	}

	adoptedBefore := l.transplantAdopted.Load()
	l.tryTransplant(fd)

	if target.calls.Load() != 1 {
		t.Fatalf("apparatus: the target was asked %d times, want 1 (the refusal is the scenario)", target.calls.Load())
	}
	if l.conns[fd] == nil || l.transplantAdopted.Load() != adoptedBefore+1 {
		t.Fatalf("apparatus: the conn was not reclaimed onto the loop (adopted %d, was %d)", l.transplantAdopted.Load(), adoptedBefore)
	}
	if l.reqBatch != 1 {
		t.Fatalf("celeris#886 item 25: the loop counted %d recvs for one read of the conn: the refused hand-off's reclaim replayed the carried bytes drainRead had counted, and counted them again", l.reqBatch)
	}
}
