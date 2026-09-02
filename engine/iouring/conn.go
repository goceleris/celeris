//go:build linux

package iouring

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris/internal/conn"
)

// maxSendQueueBytes is the per-connection back-pressure limit for
// H1/H2 connections. When pending send data exceeds this, the
// connection is closed to prevent unbounded memory growth while a
// slow peer stalls with un-ACKed responses.
//
// maxSendQueueBytesDetached is the corresponding limit once a
// connection is detached (WebSocket / SSE). Detached middleware owns
// its own flow control — ReadLimit + backpressure — and may legitimately
// echo payloads larger than 4 MiB (Autobahn 9.1.6 sends 16 MiB).
// 64 MiB matches the WS default ReadLimit.
const (
	maxSendQueueBytes         = 4 << 20  // 4 MiB (H1/H2)
	maxSendQueueBytesDetached = 64 << 20 // 64 MiB (WS/SSE)
	// maxPendingInputBytes caps the async dispatch input buffer
	// (cs.asyncInBuf) so a client pipelining requests faster than
	// the dispatch goroutine drains them cannot balloon per-conn
	// memory. Matches the send-side cap.
	maxPendingInputBytes = 4 << 20
)

// sendCap returns the effective back-pressure limit for cs, accounting
// for whether the connection is detached. Async-mode HTTP1 conns set
// detachMu up front without being truly detached; they keep the H1/H2
// limit so a stalled peer cannot balloon per-conn memory to 64 MiB.
func (cs *connState) sendCap() int {
	if cs.detachMu != nil && cs.h1State != nil && cs.h1State.Detached.Load() {
		return maxSendQueueBytesDetached
	}
	return maxSendQueueBytes
}

// iovec mirrors Linux struct iovec (16 bytes on 64-bit platforms).
// Used for IORING_OP_WRITEV scatter-gather sends. Celeris only builds
// on amd64 and arm64 (both 64-bit), so size is stable at 16.
type iovec struct {
	Base uintptr
	Len  uint64
}

type connState struct {
	fd int // 8: real FD, or fixed file index
	// generation tags every conn-bound SQE's user_data (review 2.6). It is
	// INCREMENTED on each acquireConnState (never reset on release), so a
	// reused connState/fd gets a different gen from its predecessor. A
	// late/in-flight CQE carrying the OLD gen is dropped at dispatch
	// instead of being misrouted to the new occupant (fixes the
	// use-after-reuse / wrong-conn-write / error-CQE-nils-live-slot class:
	// 2.2 error path, 2.7 hijack/close fd-reuse).
	//
	// uint16, widened from uint8 in v1.5.0: generations are per-connState
	// OBJECT (pool-recycled), not per-fd, so the conn that re-occupies an
	// fd draws its gen from a different counter and can collide with a
	// closed predecessor that still has terminal CQEs in flight. A
	// collision misroutes those CQEs to the live conn at dispatch
	// (staleConnCQE), misdecrementing its kernelInflight — at 1→0 that
	// disarms the close-time cancel and re-opens the early-release UAF
	// this release fixed. 16 bits drops the per-reuse collision odds from
	// 1/256 to 1/65536; wrap-around remains fine — 65536 generations
	// outlast any in-flight CQE.
	generation uint16 // 2
	// liveIdx is this conn's index into Worker.liveConns, maintained so
	// removeLiveConn is O(1) (swap-with-last) instead of an O(N) linear
	// scan (celeris#318 follow-up / v1.5.0 review 1.8). -1 when the conn is
	// not in liveConns (freshly acquired / released). Worker-thread-only.
	liveIdx int // 8
	// protocol is accessed concurrently: the worker reads it on every
	// recv completion, and the async dispatch goroutine writes it once
	// via switchToH2Local during an H1→H2 upgrade. atomic.Int32 keeps
	// the common-case Load a single mov instruction while making the
	// write race-free. Use cs.getProtocol / cs.setProtocol helpers.
	protocol       atomic.Int32 // engine.Protocol values cast to int32
	detected       bool         // 1
	sending        bool         // 1: true when a SEND SQE is in-flight
	closing        bool         // 1: defers close until sends complete
	dirty          bool         // 1: true when data needs flushing
	fixedFile      bool         // 1: true when fd is fixed file index
	recvLinked     bool         // 1: RECV was linked to SEND (skip standalone prepareRecv)
	needsRecv      bool         // 1: recv arm was dropped (SQ ring full); retry on next opportunity
	recvIntoBody   bool         // 1: next recv CQE fills h1State.bodyBuf directly (skips ProcessH1 + cs.buf memcpy)
	zcNotifPending bool         // 1: waiting for SEND_ZC notification CQE
	zcSentBytes    int32        // bytes sent from first SEND_ZC CQE (processed on NOTIF)
	sendBuf        []byte       // 24: in-flight buffer (accessed with sending flag)

	writeBuf []byte // 24: append buffer for handler writes
	bodyBuf  []byte // 24: zero-copy body slice; sent as iovec[1] alongside sendBuf
	sendBody []byte // 24: in-flight body slice during WRITEV (cleared by completeSend)
	buf      []byte // 24: per-connection recv buffer
	// detectAccum accumulates the first bytes of a connection across
	// multiple recvs while protocol detection is still inconclusive
	// (ErrInsufficientData). Single-shot recv re-arms into cs.buf at offset
	// 0 each time, and the bufRing path returns each provided buffer, so
	// without accumulating here a multi-recv H2 client preface (24 bytes
	// split across packets) would lose its earlier bytes (v1.5.0 review
	// 2.8). Empty/nil once cs.detected is set; only the slow split-preface
	// path ever allocates it.
	detectAccum []byte // 24
	// bodyRecvPin retains the H1State.bodyBuf backing array while a
	// single-shot recv has been armed directly into it (pickRecvTarget's
	// recvIntoBody path). conn.CloseH1 nils H1State.bodyBuf on close, which
	// would otherwise let GC reclaim the backing array while a kernel recv
	// SQE still targets it — the #256 use-after-free class, body-buffer
	// variant. Holding the slice here keeps the array alive until the
	// connState drains from pendingRelease (mirrors how cs.buf is held).
	bodyRecvPin []byte     // 24
	iov         [2]iovec   // 32: iovec storage for WRITEV SQEs (sendBuf + sendBody)
	dirtyNext   *connState // 8
	dirtyPrev   *connState // 8

	lastActivity int64 // nanosecond timestamp of last I/O activity (for timeout checks)

	h1State      *conn.H1State
	h2State      *conn.H2State
	ctx          context.Context
	remoteAddr   string
	writeFn      func([]byte) // cached write function (avoids closure allocation per recv)
	detachMu     *sync.Mutex  // non-nil after Detach(); guards writeBuf from event loop + goroutine
	detachClosed bool         // true after closeConn on a detached conn; writeFn becomes no-op

	// WebSocket recv backpressure (detached conns only):
	recvPaused       bool        // engine-side current state (worker-thread only)
	recvPauseDesired atomic.Bool // requested state from middleware goroutine

	// Async handler dispatch (Worker.async=true, HTTP1 only):
	// Incoming recv bytes are appended under asyncInMu by the worker.
	// A single dispatch goroutine per conn drains asyncInBuf via a
	// double-buffer swap with asyncOutBuf, runs ProcessH1 under
	// detachMu, then enqueues on detachQueue so the worker submits
	// SEND SQEs on its own goroutine (SINGLE_ISSUER). The goroutine
	// parks on asyncCond between requests rather than exiting — saves
	// the ~1.5µs spawn cost on keep-alive conns. Shape matches
	// engine/epoll's runAsyncHandler and preserves HTTP/1.1 pipelining.
	asyncInBuf  []byte
	asyncOutBuf []byte
	asyncInMu   sync.Mutex
	asyncCond   sync.Cond
	asyncRun    bool
	asyncClosed atomic.Bool
	// transplantPending (#383 reverse, async) is set by the dispatch
	// goroutine when it reaches a clean park boundary while an io_uring→epoll
	// drain is active: it marks itself for hand-off, sets asyncRun=false,
	// enqueues cs on the worker's detachQueue, and EXITS. drainDetachQueue
	// (worker thread) then does the SQE work (cancel recv) + dup + hand the
	// fd to epoll. Because the goroutine has already exited when the worker
	// acts, there is no goroutine-vs-release race. The recv feed path clears
	// this (aborting the transplant) if a new request arrives first, so no
	// request is lost. Atomic: goroutine writes under asyncInMu; the worker
	// reads it on the recv path and in drainDetachQueue. Mirrors the
	// asyncH2Promoted hand-off shape.
	transplantPending atomic.Bool
	// asyncPromoted is set once an async-marked route has been observed
	// on this conn while it ran inline on the worker (per-handler async,
	// celeris #300). Once promoted, recv goes to the dispatch goroutine.
	// REVERSIBLE (celeris#364): the dispatch goroutine clears it to revert
	// the conn to inline when the promoting route de-promotes. Atomic because
	// the worker reads it on the recv hot path while the goroutine may clear
	// it; the worker re-reads it under asyncInMu before feeding to close the
	// feed-vs-revert race. Reset on release.
	asyncPromoted atomic.Bool
	// promotedMethod/promotedPath record the route that forced this conn's
	// promotion (celeris#364). Written by the worker before the dispatch
	// goroutine starts (happens-before), read by the goroutine to decide
	// revert. Empty => not revert-eligible (e.g. promoted for a buffered
	// partial-header / chunked continuation, where no full route is known).
	promotedMethod string
	promotedPath   string
	// asyncH2Promoted signals the worker that runAsyncHandler observed
	// ErrUpgradeH2C and completed the cs-local H1→H2 state swap under
	// detachMu. drainDetachQueue finishes the promotion by appending
	// cs.fd to w.h2Conns (the worker-owned write-queue poll list) and
	// keeps the conn alive, rather than routing it through the
	// asyncClosed teardown.
	asyncH2Promoted atomic.Bool

	// asyncDetachUnlocked is set by OnDetach when it releases detachMu
	// on behalf of the dispatch goroutine. runAsyncHandler observes it
	// after ProcessH1 returns and skips the symmetric final Unlock
	// (otherwise it would unlock an already-released mutex). Cleared by
	// releaseConnState.
	//
	// Background: in async mode the dispatch goroutine takes detachMu
	// around ProcessH1 so writeBuf access serialises with the worker's
	// flushSend. After Detach, the engine swaps writeFn to a "guarded"
	// closure that re-acquires detachMu — which would deadlock if the
	// dispatch goroutine still holds it. OnDetach therefore releases
	// the lock early, and this flag prevents runAsyncHandler from
	// double-unlocking. Post-Detach writes (from the dispatch goroutine
	// itself or from spawned middleware goroutines like ws/sse) acquire
	// detachMu freshly through the guarded closure — no deadlock, no
	// race with the worker's flushSend.
	asyncDetachUnlocked bool

	// asyncDetachPending is set by OnDetach in async mode to defer the
	// worker-private bookkeeping (detachedCount++, prepareH2Poll arm)
	// to drainDetachQueue, which runs on the worker thread. The
	// dispatch goroutine MUST NOT mutate worker-owned state directly —
	// w.detachedCount races with adaptiveTimeout's read on the worker
	// thread, and the ring's SQE submission is SINGLE_ISSUER (only the
	// worker may call GetSQE). Cleared by drainDetachQueue after the
	// bookkeeping runs, and by releaseConnState on teardown.
	asyncDetachPending bool

	// headerTimerSpec is the kernelTimespec passed to IORING_OP_TIMEOUT
	// SQEs that enforce ReadHeaderTimeout per-conn. Owned by the conn
	// (rather than per-SQE allocated) so the spec memory stays valid
	// while the SQE is in the kernel queue. Re-used each arm.
	headerTimerSpec kernelTimespec
	// headerTimerArmed is true when a header-timer SQE has been submitted
	// and the corresponding CQE has not yet been processed. Used to
	// avoid duplicate timer SQEs.
	headerTimerArmed bool
	// forceRSTClose is set by the slowloris-defence close paths
	// (handleHeaderTimer + checkTimeouts HeaderDeadline branch) to
	// signal that the conn should be torn down via RST instead of the
	// usual graceful FIN+drain. finishClose / finishCloseDetached
	// honor the flag by skipping Shutdown+drainRecvBuffer entirely and
	// calling close() directly — SO_LINGER {1, 0} (set by the caller
	// before closeConn) then forces RST. The walker's next write hits
	// ECONNRESET immediately regardless of TCP send-buffer state.
	forceRSTClose bool

	// kernelInflight counts conn-buffer-referencing kernel ops — RECVs
	// targeting cs.buf / h1State.bodyBuf and SEND/WRITEV/SEND_ZC reading
	// cs.sendBuf / cs.sendBody / cs.iov — that have been submitted but
	// have not yet delivered their TERMINAL CQE (success, -ECANCELED,
	// -ECONNRESET, ...; for multishot recv and SEND_ZC the terminal CQE
	// is the one WITHOUT CQE_F_MORE). Incremented at SQE submission
	// (prepareRecv / flushSend / flushSendLink), decremented at CQE
	// dispatch (staleConnCQE — both the live and the stale-generation
	// path, the latter via Worker.closedOps). Worker-thread-only.
	//
	// This is the release gate for the #256-class use-after-free (v1.4.15/7beebb9 allocCount variant):
	// unix.Close(fd) does NOT complete a pending io_uring recv (the op
	// holds its own file reference), so a closed conn's cs.buf must stay
	// reachable until every kernel-held op has terminated — otherwise a
	// retransmitted/straggler segment is DMA'd into memory the Go runtime
	// has repurposed. drainPendingRelease only releases a connState once
	// this counter reaches zero (with a wall-clock backstop for kernel
	// anomalies). Mirrors driverConn.inflightOps.
	kernelInflight int32
	// recvArmed is true while a recv SQE (single-shot or multishot) is
	// kernel-held for this conn. Set by prepareRecv / flushSendLink's
	// linked recv, cleared when the recv's terminal CQE is dispatched.
	// The close paths use it to target an ASYNC_CANCEL at the armed
	// recv's exact generation-tagged user_data. Worker-thread-only.
	recvArmed bool
}

var connStatePool = sync.Pool{
	New: func() any {
		return &connState{
			writeBuf: make([]byte, 0, 4096),
			sendBuf:  make([]byte, 0, 4096),
		}
	},
}

func acquireConnState(ctx context.Context, fd int, bufSize int, async bool) *connState {
	cs := connStatePool.Get().(*connState)
	cs.fd = fd
	// Bump the generation on every acquire (wrap-around is fine). Not reset
	// in releaseConnState — incrementing here guarantees a reused
	// connState/fd never shares its predecessor's gen, so a stale CQE
	// stamped with the old gen is dropped at dispatch (review 2.6).
	cs.generation++
	cs.liveIdx = -1
	cs.ctx = ctx
	cs.writeBuf = cs.writeBuf[:0]
	cs.sendBuf = cs.sendBuf[:0]
	if bufSize > 0 {
		if cap(cs.buf) >= bufSize {
			cs.buf = cs.buf[:bufSize]
		} else {
			cs.buf = make([]byte, bufSize)
		}
	}
	// Async handler dispatch: pre-allocate detachMu so the dispatch
	// goroutine and the worker can serialize writeBuf access without a
	// later install step. Harmless (nil-free) when unused.
	if async {
		cs.detachMu = &sync.Mutex{}
		cs.asyncCond.L = &cs.asyncInMu
	}
	return cs
}

func releaseConnState(cs *connState) {
	cs.h1State = nil
	cs.h2State = nil
	cs.ctx = nil
	cs.writeFn = nil
	cs.remoteAddr = ""
	cs.dirtyNext = nil
	cs.dirtyPrev = nil
	cs.protocol.Store(0)
	cs.detected = false
	cs.sending = false
	cs.closing = false
	cs.dirty = false
	cs.fixedFile = false
	cs.recvLinked = false
	cs.needsRecv = false
	cs.recvIntoBody = false
	cs.zcNotifPending = false
	cs.zcSentBytes = 0
	cs.lastActivity = 0
	cs.detachMu = nil
	cs.detachClosed = false
	cs.recvPaused = false
	cs.recvPauseDesired.Store(false)
	cs.headerTimerSpec = kernelTimespec{}
	cs.headerTimerArmed = false
	cs.forceRSTClose = false
	cs.asyncInBuf = cs.asyncInBuf[:0]
	cs.asyncOutBuf = cs.asyncOutBuf[:0]
	cs.asyncRun = false
	cs.asyncClosed.Store(false)
	cs.transplantPending.Store(false)
	cs.asyncPromoted.Store(false)
	cs.promotedMethod = ""
	cs.promotedPath = ""
	cs.asyncDetachUnlocked = false
	cs.asyncDetachPending = false
	cs.bodyBuf = nil
	cs.sendBody = nil
	// bodyRecvPin is cleared here, after every kernel-held op delivered its
	// terminal CQE (releaseConnState is only called from drainPendingRelease
	// once cs.kernelInflight drained — or its backstop fired), so the kernel
	// can no longer be writing into the pinned bodyBuf array (#256
	// body-buffer UAF guard).
	cs.bodyRecvPin = nil
	cs.detectAccum = cs.detectAccum[:0]
	// kernelInflight is zero on every normal release (drainPendingRelease
	// gates on it); reset defensively for the wall-clock-backstop path,
	// where the worker gave up waiting on a CQE the kernel never produced.
	cs.kernelInflight = 0
	cs.recvArmed = false
	cs.fd = 0
	cs.liveIdx = -1
	connStatePool.Put(cs)
}
