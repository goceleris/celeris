//go:build linux

package iouring

import (
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goceleris/celeris/engine"
)

// recordingTarget stands in for the epoll engine on the receiving end of a
// transplant, so a test can tell whether the hand-off actually happened.
type recordingTarget struct {
	adopted atomic.Int64
}

func (r *recordingTarget) AdoptConn(fd int, _ engine.Carryover) error {
	r.adopted.Add(1)
	_ = unix.Close(fd)
	return nil
}

// TestFinishAsyncTransplantHoldsForInflightEgress pins celeris#529.
//
// asyncTransplantEligible runs on the dispatch goroutine and infers "the
// response is fully flushed" from an empty writeBuf. The partial-write path
// breaks that inference: it compacts a short write's remainder back into
// writeBuf, and the dirty loop's flushSend then swaps writeBuf into sendBuf
// and submits a ring SEND — leaving writeBuf empty while a SEND is in flight.
// Transplanting there dups the fd and closes the original, so the SEND's CQE
// is dropped as stale: cs.sending is never cleared and the bytes may never
// reach the wire, silently.
func TestFinishAsyncTransplantHoldsForInflightEgress(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*connState)
		wantAdopt  int64
		wantClosed bool
	}{
		{"clean", func(*connState) {}, 1, true},
		{"sending", func(cs *connState) { cs.sending = true }, 0, false},
		{"zc_notif_pending", func(cs *connState) { cs.zcNotifPending = true }, 0, false},
		{"sendbuf_nonempty", func(cs *connState) { cs.sendBuf = []byte("unsent") }, 0, false},
		{"writebuf_nonempty", func(cs *connState) { cs.writeBuf = []byte("unsent") }, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fd, other := socketPairFDs(t)
			defer func() { _ = unix.Close(other) }()

			w := newDirtyTestWorker(fd)
			tgt := &recordingTarget{}
			w.transplant.Store(&transplantTargetHolder{target: tgt})

			cs := &connState{fd: fd, liveIdx: -1}
			w.conns[fd] = cs
			w.connCount = 1
			tc.mutate(cs)

			// Without the egress check the call runs on into cancelConnOps,
			// which needs a live ring this unit Worker does not have. Recover
			// so a regression fails with the assertions below rather than a
			// raw panic that aborts the rest of the package.
			func() {
				defer func() { _ = recover() }()
				w.finishAsyncTransplant(cs)
			}()

			// The load-bearing assertion. A held transplant must leave the
			// connection registered with this worker; the unregistration
			// (removeLiveConn + w.conns[fd] = nil) is the first thing the
			// teardown does, so this catches a regression even when the
			// recover above swallows a later panic.
			stillRegistered := w.conns[fd] == cs
			if stillRegistered == (tc.wantAdopt != 0) {
				t.Errorf("conn registered after the call = %v, want %v",
					stillRegistered, tc.wantAdopt == 0)
			}
			if got := tgt.adopted.Load(); got != tc.wantAdopt {
				t.Errorf("AdoptConn called %d times, want %d", got, tc.wantAdopt)
			}
			// A held-back transplant must leave the original fd open: the
			// conn stays in place and is retried at its next park boundary.
			_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
			closed := err != nil
			if closed != tc.wantClosed {
				t.Errorf("original fd closed = %v, want %v (err=%v)", closed, tc.wantClosed, err)
			}
			if !closed {
				_ = unix.Close(fd)
			}
		})
	}
}
