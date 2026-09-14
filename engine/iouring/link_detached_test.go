//go:build linux

package iouring

import (
	"testing"

	"github.com/goceleris/celeris/internal/conn"
)

// TestFlushSendLinkNeverChainsOnDetachedConn is the celeris#607 regression
// guard.
//
// IOSQE_IO_LINK makes the kernel hold the RECV until the SEND completes. On
// the H1 request/response cycle that is free: the peer does not send the
// next request before it reads this response. On a DETACHED connection
// (WebSocket / SSE) the two directions are independent, and a peer that
// stops reading while it keeps sending — which is what backpressure is —
// blocks the send for as long as its receive window stays shut. Chained
// behind it, the connection cannot read at all: measured at up to 14 s on
// the pause_cancel oracle, with the peer's Close frame sitting unread in
// the server's receive queue and cs.recvArmed true the whole time, so no
// other arming site will (or may — celeris#484) place a second recv.
//
// Both arms matter. The detached arm asserts the chain is gone; the
// attached arm asserts it is still there, so a future change that disables
// linking outright cannot pass this test by accident.
func TestFlushSendLinkNeverChainsOnDetachedConn(t *testing.T) {
	cases := []struct {
		name string
		// h1 nil models a driver / EventLoopProvider conn, which has no
		// H1 state to be detached.
		h1         bool
		detached   bool
		wantLinked bool
		wantSQEs   uint32
	}{
		{"attached-h1", true, false, true, 2},
		{"detached-ws", true, true, false, 1},
		{"no-h1-state", false, false, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecvArmFixture(t)
			cs := f.cs
			if tc.h1 {
				h1 := &conn.H1State{}
				h1.Detached.Store(tc.detached)
				cs.h1State = h1
			}
			cs.writeBuf = append(cs.writeBuf, "HTTP/1.1 200 OK\r\n\r\n"...)

			if retry := f.w.flushSendLink(cs); retry {
				t.Fatalf("flushSendLink asked for a retry on a 64-entry ring with 2 free slots")
			}

			if cs.recvLinked != tc.wantLinked {
				t.Errorf("recvLinked = %v, want %v", cs.recvLinked, tc.wantLinked)
			}
			// recvArmed and the kernel-op accounting must agree with the
			// chain: an arm counted for a recv that was never placed would
			// make the close path wait for a CQE that never comes, and an
			// arm NOT counted for one that was placed is the celeris#256
			// early-release class.
			if cs.recvArmed != tc.wantLinked {
				t.Errorf("recvArmed = %v, want %v", cs.recvArmed, tc.wantLinked)
			}
			wantOutstanding := int8(0)
			if tc.wantLinked {
				wantOutstanding = 1
			}
			if cs.recvOutstanding != wantOutstanding {
				t.Errorf("recvOutstanding = %d, want %d", cs.recvOutstanding, wantOutstanding)
			}
			if got := f.ring.Pending(); got != tc.wantSQEs {
				t.Errorf("SQEs queued = %d, want %d (send%s)", got, tc.wantSQEs,
					map[bool]string{true: " + chained recv", false: " only"}[tc.wantLinked])
			}
			// The unchained arm must not silently drop the recv: it leaves
			// recvLinked false precisely so the caller's own
			// `!cqeHasMore && !cs.recvLinked && !cs.recvPaused` tail arms
			// one independently. Assert that tail actually places it.
			if !tc.wantLinked {
				if !f.w.prepareRecv(cs, cs.buf) {
					t.Fatal("the caller's standalone re-arm found no SQE")
				}
				if !cs.recvArmed || cs.recvOutstanding != 1 {
					t.Errorf("after the standalone re-arm: recvArmed=%v recvOutstanding=%d, want true/1",
						cs.recvArmed, cs.recvOutstanding)
				}
				if got := f.w.recvArm.doubleArmed.Load(); got != 0 {
					t.Errorf("doubleArmed = %d: the unchained path placed a second recv on top of a live one (celeris#484)", got)
				}
			}
		})
	}
}
