//go:build linux

package iouring

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"testing"

	"golang.org/x/sys/unix"
)

// celeris#884 item 24. TestAsyncResponseWaitsForAnInFlightRingSend
// (async_write_order_test.go) reaches only the cs.sending clause of
// ringSendOutstanding: its SEND is in flight, as a SEND is while the kernel
// has it. The other two clauses guard states in which no SEND is in flight, or
// not only one, and each needs its own state:
//
//   - len(cs.sendBuf) > 0 with cs.sending false: a partial SEND completion
//     kept the rest of the response in sendBuf and the SQ ring was full when
//     completeSend tried to send it again (flushSend returned true, and the
//     conn waits on the dirty list). The kernel sends nothing, the bytes are
//     owed, and a pipelined response direct-written now would land in the
//     middle of the response (arm sqfull_sendbuf, the reviewer's probe of
//     celeris#800).
//   - cs.zcNotifPending: a SEND_ZC's first completion (F_MORE) arrived and its
//     notification has not. cs.sending stays set through that window and
//     sendBuf is not trimmed until the notification, so no end to end state
//     reads the clause alone: arm zc drives the window and holds the order,
//     but it is TestRingSendOutstandingClauses that pins the clause itself.
//
// Both arms play the kernel and the client as TestAsyncResponseWaitsForAnIn-
// FlightRingSend does (orderClient751, kernelSend751).

// orderRig884 is an async conn on an fdlFixture, served /big by its dispatch
// goroutine, whose direct write was short: the worker has the rest as a ring
// SEND in flight (cs.sending, sendBuf) and the client has read what the direct
// write delivered.
func orderRig884(t *testing.T) (f *fdlFixture, client *orderClient751, head int) {
	t.Helper()
	f = newFDLFixture(t, true)
	f.w.handler = orderHandler751{}
	f.cs.asyncPromoted.Store(true)
	f.armFirstRecv()
	if err := unix.SetNonblock(f.peer, true); err != nil {
		t.Fatalf("nonblock: %v", err)
	}
	client = &orderClient751{t: t, peer: f.peer}
	t.Cleanup(func() {
		f.cs.asyncInMu.Lock()
		f.cs.asyncClosed.Store(true)
		f.cs.asyncCond.Broadcast()
		f.cs.asyncInMu.Unlock()
		f.w.asyncWG.Wait()
	})
	f.deliver("GET /big HTTP/1.1\r\nHost: x\r\n\r\n")
	waitDispatchIdle751(t, f, false)
	f.w.drainDetachQueue()
	f.w.flushDirty()
	takeSQEs(f.w.ring)
	if !f.cs.sending || len(f.cs.sendBuf) == 0 {
		t.Fatalf("apparatus: no SEND in flight after request 1 (sending=%v sendBuf=%d)", f.cs.sending, len(f.cs.sendBuf))
	}
	return f, client, client.drain()
}

// finishOrder884 has the kernel complete what is owed (and whatever the worker
// submits after it), and checks the client got response 1 whole and then
// response 2 ("ok"), with nothing of response 2 before the owed bytes.
func finishOrder884(t *testing.T, f *fdlFixture, client *orderClient751, early int, owed int, kernel func() int) {
	t.Helper()
	sends := kernel()
	client.drain()
	t.Logf("celeris884 ORDER owed=%d early=%d sends=%d total=%d", owed, early, sends, len(client.got))
	if early != 0 {
		t.Errorf("%d bytes of response 2 were direct-written ahead of the owed %d-byte remainder of response 1", early, owed)
	}
	br := bufio.NewReader(bytes.NewReader(client.got))
	r1, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("response 1: %v", err)
	}
	b1, err := io.ReadAll(r1.Body)
	if err != nil || !bytes.Equal(b1, orderBody751) {
		at := 0
		for at < min(len(b1), len(orderBody751)) && b1[at] == orderBody751[at] {
			at++
		}
		t.Fatalf("response 1 is not intact (%d bytes, err %v; first difference at byte %d: %q): the next response "+
			"was written into it", len(b1), err, at, b1[at:min(at+40, len(b1))])
	}
	r2, err := http.ReadResponse(br, nil)
	if err != nil || r2.StatusCode != 200 {
		t.Fatalf("response 2 does not follow response 1: %v", err)
	}
	if b2, err := io.ReadAll(r2.Body); err != nil || string(b2) != "ok" {
		t.Errorf("response 2 body %q, err %v; want \"ok\"", b2, err)
	}
}

// TestAsyncResponseWaitsForAnOwedRingSendRemainder884 holds the order in the
// two states ringSendOutstanding's other clauses are for.
func TestAsyncResponseWaitsForAnOwedRingSendRemainder884(t *testing.T) {
	t.Run("sqfull_sendbuf", func(t *testing.T) {
		f, client, head := orderRig884(t)
		const k = 4096
		// The kernel sends k bytes of the SEND, and the SQ ring fills before the
		// completion is processed, so the remainder cannot be re-sent.
		if n, err := unix.Write(f.fd, f.cs.sendBuf[:k]); n != k || err != nil {
			t.Fatalf("apparatus: partial kernel send wrote %d, %v", n, err)
		}
		filled := 0
		for f.w.ring.GetSQE() != nil {
			filled++
		}
		f.process(&completionEntry{UserData: encodeUserDataGen(udSend, f.fd, f.gen), Res: k})
		takeSQEs(f.w.ring) // the fillers, and anything placed: the kernel never sees them
		owed := len(f.cs.sendBuf)
		if f.cs.sending || owed == 0 || !f.cs.dirty {
			t.Fatalf("apparatus: want the remainder owed with no SEND in flight: sending=%v sendBuf=%d dirty=%v "+
				"(SQ ring filled with %d)", f.cs.sending, owed, f.cs.dirty, filled)
		}
		client.drain()

		// Request 2, answered while the remainder is owed and nothing is in flight.
		f.deliver("GET /small HTTP/1.1\r\nHost: x\r\n\r\n")
		waitDispatchIdle751(t, f, false)
		early := client.drain()
		t.Logf("celeris884 ORDER arm=sqfull_sendbuf head=%d filled=%d", head, filled)
		finishOrder884(t, f, client, early, owed, func() int { return kernelSend751(t, f, client) })
	})

	t.Run("zc", func(t *testing.T) {
		f, client, head := orderRig884(t)
		cs := f.cs
		owed := len(cs.sendBuf)

		// The kernel sends the whole SEND_ZC (the client reads whenever the socket
		// is full) and posts its first completion, F_MORE. The notification has
		// not come.
		kernelWrite := func() {
			t.Helper()
			for b := cs.sendBuf; len(b) > 0; {
				n, err := unix.Write(f.fd, b)
				if n > 0 {
					b = b[n:]
					continue
				}
				if err != unix.EAGAIN && err != unix.EINTR {
					t.Fatalf("kernel send: %v", err)
				}
				if client.drain() == 0 {
					t.Fatal("kernel send: the socket is full and the client has nothing to read")
				}
			}
		}
		kernelWrite()
		first := &completionEntry{UserData: encodeUserDataGen(udSend, f.fd, f.gen), Res: int32(owed), Flags: cqeFMore}
		f.process(first)
		if !cs.zcNotifPending {
			t.Fatalf("apparatus: the first completion left zcNotifPending=%v", cs.zcNotifPending)
		}
		client.drain()

		// Request 2, answered inside the window.
		f.deliver("GET /small HTTP/1.1\r\nHost: x\r\n\r\n")
		waitDispatchIdle751(t, f, false)
		early := client.drain()

		// The notification arrives; the staged response 2 goes out behind it.
		t.Logf("celeris884 ORDER arm=zc head=%d zcNotifPending=%v sending=%v sendBuf=%d writeBuf=%d", head,
			cs.zcNotifPending, cs.sending, len(cs.sendBuf), len(cs.writeBuf))
		finishOrder884(t, f, client, early, owed, func() int {
			f.process(&completionEntry{UserData: encodeUserDataGen(udSend, f.fd, f.gen), Flags: cqeFNotif})
			return kernelSend751(t, f, client)
		})
	})
}

// TestRingSendOutstandingClauses pins each clause of ringSendOutstanding by
// itself: the function is what the dispatch goroutine's direct writes and the
// detached conns' guarded writeFn ask before they put bytes on the wire ahead
// of a ring SEND's. One field each, since end to end the SEND_ZC clause is
// covered by the other two for as long as the worker keeps cs.sending set and
// sendBuf untrimmed until the notification (celeris#884 item 24).
func TestRingSendOutstandingClauses(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(cs *connState)
		want bool
	}{
		{"nothing", func(*connState) {}, false},
		{"staged_writeBuf_only", func(cs *connState) { cs.writeBuf = append(cs.writeBuf[:0], "x"...) }, false},
		{"sending", func(cs *connState) { cs.sending = true }, true},
		{"zcNotifPending", func(cs *connState) { cs.zcNotifPending = true }, true},
		{"sendBuf", func(cs *connState) { cs.sendBuf = append(cs.sendBuf[:0], "x"...) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := &connState{}
			tc.set(cs)
			if got := ringSendOutstanding(cs); got != tc.want {
				t.Errorf("ringSendOutstanding = %v with only %s set, want %v (sending=%v zcNotifPending=%v sendBuf=%d writeBuf=%d)",
					got, tc.name, tc.want, cs.sending, cs.zcNotifPending, len(cs.sendBuf), len(cs.writeBuf))
			}
		})
	}
}
