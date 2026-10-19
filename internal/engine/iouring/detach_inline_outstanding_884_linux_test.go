//go:build linux

package iouring

import (
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/wakefd"
)

// TestDetachedInlineWriteHonoursRingSendOutstanding pins celeris#884 item 25:
// the guarded write hook of a detached conn (OnDetach) writes writeBuf to the
// socket itself, on the dispatch goroutine, only when no ring SEND of the
// conn's earlier bytes is in flight or owed, which is ringSendOutstanding's
// answer (celeris#751): the two writes would otherwise reach the wire in the
// wrong order. The hook used to keep its own copy of that condition, so a
// clause added to one was missing from the other.
//
// It drives the hook on a real socketpair, one row per field the helper reads.
// A refused inline write leaves the bytes in writeBuf for the worker's
// flushSend, which the detach queue is told about; an allowed one puts them on
// the peer's socket at once and tells nobody.
func TestDetachedInlineWriteHonoursRingSendOutstanding(t *testing.T) {
	const payload = "detached frame"
	for _, tc := range []struct {
		name   string
		set    func(cs *connState)
		inline bool
	}{
		{"nothing_outstanding", func(*connState) {}, true},
		{"sending", func(cs *connState) { cs.sending = true }, false},
		{"zcNotifPending", func(cs *connState) { cs.zcNotifPending = true }, false},
		{"sendBuf", func(cs *connState) { cs.sendBuf = append(cs.sendBuf, "earlier bytes"...) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				t.Fatalf("socketpair: %v", err)
			}
			local, peer := pair[0], pair[1]
			t.Cleanup(func() { _ = unix.Close(local); _ = unix.Close(peer) })

			w := &Worker{detachedConns: new(atomic.Int64), wakeFD: wakefd.New(-1), bytesWritten: new(atomic.Uint64)}
			cs := &connState{fd: local}
			cs.protocol.Store(int32(engine.HTTP1))
			cs.writeFn = w.makeWriteFn(cs)
			w.initProtocol(cs)
			if cs.h1State == nil || cs.h1State.OnDetach == nil {
				t.Fatal("initProtocol installed no OnDetach")
			}
			cs.h1State.OnDetach()
			if cs.detachMu == nil {
				t.Fatal("OnDetach installed no detachMu")
			}
			tc.set(cs)

			cs.writeFn([]byte(payload))

			buf := make([]byte, 64)
			n, _, rerr := unix.Recvfrom(peer, buf, unix.MSG_DONTWAIT)
			w.detachQMu.Lock()
			queued := len(w.detachQueue)
			w.detachQMu.Unlock()

			if tc.inline {
				if rerr != nil || string(buf[:n]) != payload {
					t.Errorf("no ring SEND outstanding: the peer read %q (err %v), want %q written inline", buf[:max(n, 0)], rerr, payload)
				}
				if len(cs.writeBuf) != 0 || queued != 0 {
					t.Errorf("inline write: writeBuf holds %d bytes and %d conns are queued, want none of either", len(cs.writeBuf), queued)
				}
				return
			}
			if rerr != unix.EAGAIN {
				t.Errorf("a ring SEND is outstanding (%s): the peer read %q (err %v), the hook wrote inline ahead of it", tc.name, buf[:max(n, 0)], rerr)
			}
			if string(cs.writeBuf) != payload || queued != 1 {
				t.Errorf("refused inline write: writeBuf %q and %d conns queued, want %q left for the worker's flush and one queued", cs.writeBuf, queued, payload)
			}
		})
	}
}
