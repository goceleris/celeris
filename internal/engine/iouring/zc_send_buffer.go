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
// (closedOpsEntry.zcOwed), and past the backstop the array is held until the
// notification (closedZCOwed, holdZCPastBackstop). Nothing but the peer
// decides how long that is. A peer that keeps reading, however slowly, keeps
// the orphaned socket alive; one that stays at a zero window was measured
// keeping it 5 min 36 s. Neither a descriptor (celeris#798) nor a connection
// slot counts a hold. So the hold is built to cost as little as it can for as
// long as it lasts, and to stop growing (celeris#813 round 2):
//
//   - Off the release walk. A held array waits in Worker.zcHolds, keyed by
//     its closed identity, not in pendingRelease, which the loop walks every
//     pass. Only a CQE of that identity reaches it (settleZCHolds, from
//     noteStaleTerminalOp), so a hold costs O(1) when it starts, at each of
//     its identity's CQEs and when it ends, and nothing per loop pass.
//   - The array alone. The kernel reads nothing of the connState but the
//     send buffer's array, so once the SEND_ZC is all the identity owes, the
//     connState is released as usual (to the pool without the array, or,
//     detached, to the GC) and the hold keeps the array only
//     (releaseHeldConnState). While another op is owed too (a recv the close's
//     cancel has not ended yet, which may still write cs.buf), the whole entry
//     is held, and handed back to the walk at that op's CQE.
//   - Shown. CloseZCNotifHeldNow and CloseZCNotifHeldBytes are gauges of
//     what is held right now.
//   - Bounded. A worker whose held arrays reach zcHoldBytesMax arms no new
//     SEND_ZC (prepSendSQE): its sends copy, and a copied send's unsent tail
//     is the kernel's own socket memory, which the kernel bounds itself. The
//     held bytes can then grow only by the SEND_ZCs already in flight on live
//     connections, which the connection limit bounds.
//
// Worker shutdown, whose ring teardown ends no such use of the pages either,
// keeps every send buffer still owed for the life of the process
// (retainZCSendBufsAtShutdown).
//
// Counted, in handoffLossStats and EngineMetrics:
//
//   - CloseZCNotifHeld: holds the backstop started. A rate of connections
//     closed on a stalled peer mid-send.
//   - CloseZCNotifHeldNow, CloseZCNotifHeldBytes: gauges, the arrays held
//     right now and their capacity.
//   - CloseZCNotifForced: identities dropped while they still owed a SEND_ZC
//     (dropClosedOps), i.e. a send buffer given up while the kernel may still
//     send from it. Must stay 0. The hold is decided on the identity, so the
//     backstop never drops one that owes a SEND_ZC: this is a tripwire for a
//     change that breaks that, not a measure of the kernel.
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

// closedZCOwed reports whether closed cs's identity still owes a SEND_ZC,
// the send or its notification, so the kernel may still read a send buffer
// closed under it: its closedOps entry counts one (closedOpsEntry.zcOwed).
// It is decided on the identity, not on cs's own send: under an (fd,
// generation) collision the accounting cannot tell the conns' CQEs apart, so
// a conn with no SEND_ZC of its own is held with its sibling's, and none of
// them reaches the backstop's release and drops the identity while the
// sibling's array is still read. No entry, or kernelInflight at zero, means
// the accounting has retired the identity: every op delivered its terminal
// CQE. Worker thread only.
func (w *Worker) closedZCOwed(cs *connState) bool {
	if cs.kernelInflight <= 0 {
		return false
	}
	e := w.closedOps[encodeConnOpKey(cs.fd, cs.generation)]
	return e != nil && e.zcOwed > 0
}

// zcHold is a send buffer the release backstop holds for a SEND_ZC past its
// deadline, in Worker.zcHolds, off the release walk (celeris#812, #813).
type zcHold struct {
	// sendBuf is the array the kernel may read, at its full capacity: what
	// the hold keeps, and what it counts in bytes.
	sendBuf []byte
	// entry is the whole pendingRelease entry while its identity owes an op
	// other than a SEND_ZC too, which may still write the connState's other
	// buffers: settleZCHolds hands it back to the walk at that identity's
	// next CQE. Zero (cs nil) once the SEND_ZC is all that is owed: the
	// connState has been released, and the hold keeps the array alone.
	entry pendingReleaseEntry
}

// zcHoldBytesMax is how many bytes of send buffers a worker holds past the
// backstop before it stops arming SEND_ZC (prepSendSQE): a hold lasts as long
// as the peer keeps its orphaned socket alive, and no descriptor or connection
// slot counts it, so this is what bounds it. At or above it the worker's sends
// copy, and a copied send's unsent tail is the kernel's own socket memory,
// which its orphan and socket-memory limits bound. 16 MiB is some 2,000
// held responses of 8 KiB per worker, far above the few a server closes on
// peers that stopped reading mid-send, and a small share of memory.
const zcHoldBytesMax = 16 << 20

// holdZCPastBackstop takes e, which the backstop found still owing a
// SEND_ZC (closedZCOwed), off the release walk and holds its send buffer in
// Worker.zcHolds until the SEND_ZC's notification (settleZCHolds). The
// caller does not keep e. The descriptor is not held with it. A
// notification names none, and the entry let go of it already (celeris#798);
// if it still keeps it, an op that does name it is owed past the backstop
// (the SEND_ZC's send itself, or a recv), and it is closed and counted as the
// backstop has always done (CloseFDForced, must stay 0). If the SEND_ZC is
// all the identity owes, the connState goes now (releaseHeldConnState) and
// the hold keeps the array alone; otherwise the whole entry is held. The
// hold is counted once per entry (CloseZCNotifHeld), however often its entry
// comes back to the walk. Worker thread only.
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
	if !e.zcHeld {
		e.zcHeld = true
		w.handoffLoss.noteCloseZCNotifHeld()
		if w.logger != nil {
			w.logger.Debug("holding a closed connection's send buffer past the release backstop until its SEND_ZC notification",
				"worker", w.id, "fd", cs.fd, "generation", cs.generation,
				"inflight", cs.kernelInflight, "detached", e.detached)
		}
	}
	key := encodeConnOpKey(cs.fd, cs.generation)
	h := zcHold{sendBuf: cs.sendBuf[:cap(cs.sendBuf)], entry: *e}
	if ce := w.closedOps[key]; ce != nil && ce.inflight == int32(ce.zcOwed) {
		w.releaseHeldConnState(ce, cs, e.detached)
		h.entry = pendingReleaseEntry{}
	}
	if w.zcHolds == nil {
		w.zcHolds = make(map[uint64][]zcHold)
	}
	w.zcHolds[key] = append(w.zcHolds[key], h)
	n := cap(h.sendBuf)
	w.zcHoldCount++
	w.zcHoldBytes += n
	w.handoffLoss.addZCHeld(1, int64(n))
}

// releaseHeldConnState releases held cs, whose identity ce owes nothing but
// SEND_ZC ops, while the hold keeps its send buffer's array: the kernel reads
// nothing else of it (a SEND_ZC's SQE carries the array's address and
// length; it is never a WRITEV, and nothing owed writes cs.buf). cs leaves
// the identity first, its slot set to nil so the number of conns the
// collision rule reads is kept (noteStaleTerminalOp), and so no later CQE of
// the identity writes into a connState the pool has handed on. A plain one
// then goes to the pool without the array, which its next owner would write
// into; a detached one to the GC as it is, since a dispatch goroutine may
// still read it (queuePendingReleaseDetached). Worker thread only.
func (w *Worker) releaseHeldConnState(ce *closedOpsEntry, cs *connState, detached bool) {
	for i, c := range ce.conns {
		if c == cs {
			ce.conns[i] = nil
		}
	}
	if detached {
		return
	}
	cs.sendBuf = nil
	releaseConnState(cs)
}

// settleZCHolds is what a CQE of closed identity key does to the send
// buffers held for it, once noteStaleTerminalOp has taken the CQE off e's
// counts. An array held alone is let go once the identity owes no SEND_ZC
// (zcOwed 0) or nothing at all: the kernel is done with it, and the GC takes
// it. A whole entry goes back to the release walk at any such CQE, where the
// next pass releases it (the identity retired), holds it again (the array
// alone, if the SEND_ZC is all that is left) or, if its SEND_ZC has ended
// with another op still owed, gives up on it as the backstop always has.
// Worker thread only.
func (w *Worker) settleZCHolds(key uint64, e *closedOpsEntry) {
	hs, ok := w.zcHolds[key]
	if !ok {
		return
	}
	done := e.inflight <= 0 || e.zcOwed == 0
	kept := hs[:0]
	for _, h := range hs {
		if h.entry.cs == nil && !done {
			kept = append(kept, h)
			continue
		}
		n := cap(h.sendBuf)
		w.zcHoldCount--
		w.zcHoldBytes -= n
		w.handoffLoss.addZCHeld(-1, -int64(n))
		if h.entry.cs != nil {
			w.pendingRelease = append(w.pendingRelease, h.entry)
		}
	}
	clear(hs[len(kept):])
	if len(kept) == 0 {
		delete(w.zcHolds, key)
	} else {
		w.zcHolds[key] = kept
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
// (liveZCOwed), of one queued for release (closedZCOwed) or held past the
// backstop (zcHolds), goes into zcRetained, which is never freed, and is
// counted (ShutdownZCBufRetained). No wait is added for them: the run loop's
// send drain (bounded by the shutdown's budget, celeris#806) already gave
// the live connections' sends their time, and a notification owed to a
// stalled peer can take minutes. Only when there is something to keep does it first take what the
// kernel has already completed: an enter that submits nothing
// (shutdownDrivers closed the driver descriptors on the promise that nothing
// is submitted after it) and waits at most zcShutdownFlushWait, which also
// runs the task work a DEFER_TASKRUN ring holds its completions in until it
// is entered; then it retires the recv and send CQEs through staleConnCQE as
// the loop does, and closes any accepted descriptor, as endOwedOpsAtShutdown
// does. The cost is the kept arrays' memory, for a shutdown that finds peers
// stalled mid-send. Either way the worker then takes its share out of the
// held-now gauges: it holds nothing any more. Worker thread only.
func (w *Worker) retainZCSendBufsAtShutdown() {
	defer w.retractZCHolds()
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

// retractZCHolds takes a shut-down worker's holds out of the held-now gauges
// and drops them: retainZCSendBufsAtShutdown has kept every array still owed.
// Worker thread only.
func (w *Worker) retractZCHolds() {
	w.handoffLoss.addZCHeld(-int64(w.zcHoldCount), -int64(w.zcHoldBytes))
	w.zcHolds, w.zcHoldCount, w.zcHoldBytes = nil, 0, 0
}

// zcShutdownFlushWait bounds the one enter retainZCSendBufsAtShutdown makes
// to collect the completions the kernel already has. It waits only while
// nothing at all has completed.
const zcShutdownFlushWait = time.Millisecond

// zcSendBufsOwed lists the send buffers a SEND_ZC may still read: of the live
// connections, of those queued for release, and those held past the backstop,
// every one of which is owed. Worker thread only.
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
	for _, hs := range w.zcHolds {
		for _, h := range hs {
			bufs = append(bufs, h.sendBuf)
		}
	}
	return bufs
}
