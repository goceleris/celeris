//go:build linux

package iouring

import (
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The send buffer of a SEND_ZC (celeris#812).
//
// A SEND_ZC does not copy cs.sendBuf into the socket: the queued segments
// reference its pages, and the kernel reads them whenever it transmits one,
// a retransmission included. Its notification CQE, the op's terminal CQE, says
// that no segment references them any more. Until then the array must not be
// written, which on a live connection flushSend and flushSendLink see to (they
// wait out cs.zcNotifPending), and must not be given to anyone else, which is
// what the release gate on a closed connection is for (drainPendingRelease,
// on cs.kernelInflight, which counts the SEND_ZC until its notification).
//
// That gate had a wall-clock backstop that released anyway 5 s after the
// close, on the reasoning that an op still owed by then is a kernel anomaly.
// A notification is not one. A peer that stops reading keeps the unsent tail
// of the send queued behind its closed window, and a close leaves the socket
// orphaned with that tail still queued: the kernel keeps offering it for as
// long as it keeps the orphan, and posts the notification when it gives up on
// the socket or the peer takes the bytes. The backstop returned such a
// connState to connStatePool with its sendBuf (a detached one to the GC); the
// array's next owner wrote into it; and when the peer read again, it received
// the tail from that array, another connection's bytes. Measured by
// TestBackstopHoldsASendBufferAZCNotificationStillReads: 61,200 of the 65,536
// bytes of the send, in every case, on the tree before this fix.
//
// So a closed connection's closedOps entry counts its SEND_ZC apart
// (closedOpsEntry.zcOwed), and the backstop holds an entry that still owes
// one until its notification (closedZCOwed, holdZCPastBackstop). The kernel
// bounds the hold: the notification comes when the peer reads or when the
// kernel ends the orphaned socket. It costs the connState and its buffers,
// never a descriptor (celeris#798). Worker shutdown, whose ring teardown ends
// no such use of the pages either, keeps every send buffer still owed for the
// life of the process (retainZCSendBufsAtShutdown).
//
// Counted, in handoffLossStats and EngineMetrics:
//
//   - CloseZCNotifHeld: entries the backstop held for a SEND_ZC. A rate of
//     connections closed on a stalled peer mid-send.
//   - CloseZCNotifForced: closed connections whose accounting was dropped
//     while it still owed a SEND_ZC (dropClosedOps), i.e. a send buffer given
//     up while the kernel may still send from it. Must stay 0.
//   - ShutdownZCBufRetained: send buffers worker shutdown kept for the life
//     of the process.

// zcSendOwed reports whether cs's send in flight is a SEND_ZC: armed as one
// (sendIsZC, set per send by prepSendSQE) and not finished, which for a
// SEND_ZC is its notification (handleSend clears zcNotifPending there, and
// completeSend then cs.sending; a first CQE that failed clears sending and
// leaves zcNotifPending set). On a closed connection the flags are the ones
// it had at the close: nothing touches them after it. Worker thread.
func zcSendOwed(cs *connState) bool {
	return cs.sendIsZC && (cs.sending || cs.zcNotifPending)
}

// closedZCOwed reports whether closed cs still owes a SEND_ZC, the send or
// its notification, so the kernel may still read cs.sendBuf: its closedOps
// entry counts one (closedOpsEntry.zcOwed). No entry, or kernelInflight at
// zero, means the accounting has retired the identity: every op delivered its
// terminal CQE, or a backstop dropped it. Worker thread only.
func (w *Worker) closedZCOwed(cs *connState) bool {
	if !cs.sendIsZC || cs.kernelInflight <= 0 {
		return false
	}
	e := w.closedOps[encodeConnOpKey(cs.fd, cs.generation)]
	return e != nil && e.zcOwed > 0
}

// holdZCPastBackstop keeps e, which the backstop found still owing a SEND_ZC
// (closedZCOwed), in pendingRelease: drainPendingRelease then releases it
// the usual way once the SEND_ZC's notification has retired the last op.
// The descriptor is not held with it. A notification names none, and the
// entry let go of it already (celeris#798); if it still keeps it, an op that
// does name it is owed past the backstop (the SEND_ZC's send itself, or a
// recv), and it is closed and counted as the backstop has always done
// (CloseFDForced, must stay 0). The hold is counted once per entry
// (CloseZCNotifHeld). Worker thread only.
func (w *Worker) holdZCPastBackstop(e *pendingReleaseEntry) {
	cs := e.cs
	if e.holdsFD {
		if w.logger != nil {
			w.logger.Warn("closing a kept descriptor with kernel ops unaccounted for after backstop hold",
				"worker", w.id, "fd", cs.fd, "generation", cs.generation,
				"inflight", cs.kernelInflight, "detached", e.detached)
		}
		w.handoffLoss.noteCloseFDForced()
		w.releaseKeptFD(e)
	}
	if e.zcHeld {
		return
	}
	e.zcHeld = true
	w.handoffLoss.noteCloseZCNotifHeld()
	if w.logger != nil {
		w.logger.Debug("holding a closed connState past the release backstop until its SEND_ZC notification",
			"worker", w.id, "fd", cs.fd, "generation", cs.generation,
			"inflight", cs.kernelInflight, "detached", e.detached)
	}
}

// liveZCOwed is zcSendOwed for a live connection at worker shutdown, whose
// flags lag its count: the shutdown drains (endOwedOpsAtShutdown, and
// retainZCSendBufsAtShutdown's reap) retire its CQEs through staleConnCQE,
// which takes each terminal one off kernelInflight (and clears recvArmed at a
// recv's) but leaves the send flags as they were. kernelInflight counts the
// armed recv, if any, and the send, so the send is still owed while it counts
// more than the recv.
func liveZCOwed(cs *connState) bool {
	n := cs.kernelInflight
	if cs.recvArmed {
		n--
	}
	return zcSendOwed(cs) && n > 0
}

// zcRetained holds, for the life of the process, the send buffers worker
// shutdown found a SEND_ZC may still read (retainZCSendBufsAtShutdown).
var zcRetained struct {
	mu   sync.Mutex
	bufs [][]byte
}

// retainZCSendBufsAtShutdown is worker shutdown's half of celeris#812, run
// just before the ring is closed. Closing the ring ends no SEND_ZC's use of
// its send buffer (the queued segments hold the pages, and the orphaned socket
// keeps sending from them), and once the ring is closed no notification can
// say when that use ends. The connStates are not pooled at shutdown, but the
// Worker is the last thing that holds them, and the engine may be dropped
// and collected long before the kernel lets go: the GC would then hand the
// array to new allocations while a peer can still receive it.
//
// So every send buffer a SEND_ZC may still read, of a live connection
// (liveZCOwed) or of one queued for release (closedZCOwed), goes into
// zcRetained, which is never freed, and is counted (ShutdownZCBufRetained).
// No wait is added for them: the run loop's send drain
// (shutdownSendDrainNanos) already gave the live connections' sends their
// time, and a notification owed to a stalled peer can take minutes. Only when
// there is something to keep does it first take what the kernel has already
// completed: an enter that submits nothing (shutdownDrivers closed the driver
// descriptors on the promise that nothing is submitted after it) and waits at
// most zcShutdownFlushWait, which also runs the task work a DEFER_TASKRUN
// ring holds its completions in until it is entered; then it retires the
// recv and send CQEs through staleConnCQE as the loop does, and closes any
// accepted descriptor, as endOwedOpsAtShutdown does. The cost is the kept
// arrays' memory, for a shutdown that finds peers stalled mid-send. Worker
// thread only.
func (w *Worker) retainZCSendBufsAtShutdown() {
	bufs := w.zcSendBufsOwed()
	if len(bufs) == 0 {
		return
	}
	if w.ring != nil && !w.sqpoll {
		_ = w.ring.WaitCQETimeout(zcShutdownFlushWait)
		head, tail := w.ring.BeginCQ()
		for ; head != tail; head++ {
			c := w.ring.cqeAt(head)
			ud := c.UserData
			switch ud & udMask {
			case udRecv, udSend:
				w.staleConnCQE(c, int(ud&fdMask), ud)
			case udAccept:
				if c.Res >= 0 && !w.fixedFiles {
					_ = unix.Close(int(c.Res))
				}
			}
		}
		w.ring.EndCQ(head)
		bufs = w.zcSendBufsOwed()
		if len(bufs) == 0 {
			return
		}
	}
	zcRetained.mu.Lock()
	zcRetained.bufs = append(zcRetained.bufs, bufs...)
	zcRetained.mu.Unlock()
	w.handoffLoss.noteShutdownZCBufRetained(uint64(len(bufs)))
	if w.logger != nil {
		w.logger.Info("keeping send buffers a SEND_ZC may still read for the life of the process",
			"worker", w.id, "buffers", len(bufs))
	}
}

// zcShutdownFlushWait bounds the one enter retainZCSendBufsAtShutdown makes
// to collect the completions the kernel already has. It waits only while
// nothing at all has completed.
const zcShutdownFlushWait = time.Millisecond

// zcSendBufsOwed lists the send buffers a SEND_ZC may still read, of the live
// connections and of those queued for release. Worker thread only.
func (w *Worker) zcSendBufsOwed() [][]byte {
	var bufs [][]byte
	for _, fd := range w.liveConns {
		if cs := w.conns[fd]; cs != nil && liveZCOwed(cs) {
			bufs = append(bufs, cs.sendBuf)
		}
	}
	for i := range w.pendingRelease {
		if cs := w.pendingRelease[i].cs; cs != nil && w.closedZCOwed(cs) {
			bufs = append(bufs, cs.sendBuf)
		}
	}
	return bufs
}

// zcHold and zcHoldBytesMax: STUB (celeris#813 round 2, failing-first
// commit), declared so the tests compile; the next commit fills them.
type zcHold struct {
	sendBuf []byte
	entry   pendingReleaseEntry
}

const zcHoldBytesMax = 16 << 20
