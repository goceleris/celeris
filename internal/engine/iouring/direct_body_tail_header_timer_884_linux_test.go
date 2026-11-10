//go:build linux

package iouring

import (
	"strconv"
	"strings"
	"testing"
)

// celeris#884 item 70 follow-up (review of the direct-body tail): on an
// AsyncHandlers engine OnHeaderDeadlineArmed is nil, so the worker arms the
// kernel header timer itself, after the request it has run. handleRecv's
// ordinary tail retries that arm (a head that ends inside the recv leaves a
// deadline pending); the direct-body block, which hands the bytes behind a
// body to ProcessH1 and so can end inside the next head too, had no such
// retry, and only the checkTimeouts sweep enforced the deadline until the
// conn's next recv.
//
// Run on a fixture whose ring the kernel never sees, so the SQEs the worker
// places are the observation: the script is celeris#908's (a 20000-byte POST so
// bodyBuf has room, then a 2000-byte POST whose last recv, received straight
// into bodyBuf, carries the start of the next head).
func TestDirectBodyTailArmsTheHeaderTimerOfAPartialHead884(t *testing.T) {
	f := headerTimerFixture762(t, false)
	post := func(n int) string {
		return "POST /p HTTP/1.1\r\nHost: x\r\nContent-Length: " + strconv.Itoa(n) + "\r\n\r\n"
	}
	directBody := func(s string) {
		t.Helper()
		if !f.cs.recvIntoBody || len(f.cs.bodyRecvPin) < len(s) {
			t.Fatalf("apparatus: no direct-body recv armed for %d bytes (recvIntoBody=%v, room %d)", len(s), f.cs.recvIntoBody, len(f.cs.bodyRecvPin))
		}
		n := copy(f.cs.bodyRecvPin, s)
		f.process(f.recvCQE(int32(n)))
	}
	big := strings.Repeat("x", 20000)
	f.deliver(post(len(big)) + big[:3000]) // the fixture's recv buffer is 4096 bytes
	directBody(big[3000:])
	small := strings.Repeat("y", 2000)
	f.deliver(post(len(small)) + small[:1000])
	// The accept-time timer is gone (it fired, or the SQ ring dropped its arm
	// and the flag was cleared), and nothing is pending on the ring.
	f.cs.headerTimerArmed = false
	takeSQEs(f.w.ring)
	if f.cs.h1State.HeaderDeadlineNs.Load() != 0 {
		t.Fatalf("apparatus: a header deadline is pending before the head starts")
	}
	directBody(small[1000:] + "GET /x HTTP/1.1\r\nHo")
	if f.cs.h1State.HeaderDeadlineNs.Load() == 0 {
		t.Fatalf("apparatus: the partial head left no header deadline pending")
	}
	if f.cs.asyncPromoted.Load() {
		t.Fatalf("apparatus: the conn was promoted by a partial head")
	}
	timers, _ := timers762(takeSQEs(f.w.ring)) // the POST's SEND and the next RECV are placed too
	if len(timers) != 1 || timers[0].ud != f.timerUD() || !f.cs.headerTimerArmed {
		t.Fatalf("celeris#884: the direct-body tail that ended inside a head placed %d header TIMEOUTs (headerTimerArmed=%v), "+
			"want one tagged for fd %d: only the sweep enforces the deadline", len(timers), f.cs.headerTimerArmed, f.fd)
	}
}
