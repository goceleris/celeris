//go:build linux

package iouring

import (
	"context"
	"math"
	"sync"
	"sync/atomic"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/recvtheft"
)

// maxSendQueueBytes is the per-connection back-pressure limit for
// H1 connections. When pending send data exceeds this, the
// connection is closed to prevent unbounded memory growth while a
// slow peer stalls with un-ACKed responses. It is held per request, not
// per write (celeris#761): a request that finds more than this unsent is not
// served (conn.H1State.WriteBacklogged), while a response, however large, is
// staged whole.
//
// maxSendQueueBytesDetached is the corresponding limit once a
// connection is detached (WebSocket / SSE). Detached middleware owns
// its own flow control — ReadLimit + backpressure — and may legitimately
// echo payloads larger than 4 MiB (Autobahn 9.1.6 sends 16 MiB).
// 64 MiB matches the WS default ReadLimit.
const (
	maxSendQueueBytes         = 4 << 20  // 4 MiB (H1)
	maxSendQueueBytesDetached = 64 << 20 // 64 MiB (WS/SSE)
	// maxSendQueueBytesH2 is the limit for an HTTP/2 connection. Its DATA
	// is already bounded by the flow-control windows the peer grants (what
	// the windows refuse waits in the streams' buffers, bounded per
	// connection by stream.OutboundBudget, celeris#893; a StreamWriter on
	// the worker pool waits for the window instead of queueing past it, and
	// one on the event loop buffers what the window refuses on its stream,
	// charged to that budget but not refused by it, celeris#904), and a
	// peer that reads keeps up to a window of frames queued, or in a SEND
	// in flight, as a matter of course (net/http's client grants 4 MiB per
	// stream, browsers more per connection), so the H1 limit closed healthy
	// HTTP/2 connections at the window's edge (celeris#761). 64 MiB, as for
	// a detached connection, still bounds a peer that grants large windows
	// and stops reading.
	maxSendQueueBytesH2 = 64 << 20
	// maxPendingInputBytes caps the async dispatch input buffer
	// (cs.asyncInBuf) so a client pipelining requests faster than
	// the dispatch goroutine drains them cannot balloon per-conn
	// memory. Matches the send-side cap.
	maxPendingInputBytes = 4 << 20
)

// sendCap returns the per-write back-pressure limit for cs: a write is
// refused when the bytes still queued before it are over it. An HTTP/2 conn
// has maxSendQueueBytesH2 and a truly-detached one (WS/SSE)
// maxSendQueueBytesDetached. An HTTP/1 conn has none: its writes are its
// handlers' responses, and a limit per write cut a response off mid-body, a
// large body or the chunks of a StreamWriter (celeris#761). Its limit,
// maxSendQueueBytes, is held per request instead (overBacklogH1), before the
// handler runs. Async-mode HTTP1 conns set detachMu up front without being
// truly detached; they are HTTP/1 conns here.
func (cs *connState) sendCap() int {
	if cs.h2State != nil {
		return maxSendQueueBytesH2
	}
	if cs.detachMu != nil && cs.h1State != nil && cs.h1State.Detached.Load() {
		return maxSendQueueBytesDetached
	}
	return math.MaxInt
}

// overBacklogH1 is the HTTP/1 back-pressure limit (conn.H1State.WriteBacklogged,
// celeris#761): whether the responses cs still has unsent, queued or in a
// SEND in flight, are over maxSendQueueBytes, i.e. its client stopped
// reading while it kept sending requests. The next request is then not
// served, and the conn is closed once what is queued has gone out.
func (cs *connState) overBacklogH1() bool {
	return len(cs.writeBuf)+len(cs.sendBuf) > maxSendQueueBytes
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
	generation uint32 // 4
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
	writeRefused   bool         // 1: a write hook refused bytes on back-pressure; the conn is closed (celeris#761, makeWriteFn)
	dirty          bool         // 1: true when data needs flushing
	fixedFile      bool         // 1: true when fd is fixed file index
	recvLinked     bool         // 1: RECV was linked to SEND (skip standalone prepareRecv)
	needsRecv      bool         // 1: recv arm was dropped (SQ ring full); retry on next opportunity
	recvIntoBody   bool         // 1: next recv CQE fills h1State.bodyBuf directly (skips ProcessH1 + cs.buf memcpy)
	zcNotifPending bool         // 1: waiting for SEND_ZC notification CQE
	h2GoAwaySent   bool         // 1: a graceful shutdown sent this H2 conn its GOAWAY (celeris#759)
	// sendIsZC records how the send SQE currently in flight for this
	// connection was ARMED: true for IORING_OP_SEND_ZC, false for a plain
	// SEND / WRITEV / linked SEND. It is the provenance flag the error
	// classification in handleSend and completeSend keys on (celeris#609).
	//
	// w.sendZC cannot serve that purpose: it is the worker's forward-looking
	// capability bit, and the EINVAL/ENOMEM fallbacks clear it while other
	// connections' ZC sends are still in flight. Classifying on it meant the
	// FIRST failing ZC completion per worker was absorbed and every sibling
	// behind it was read as a broken connection and closed. Written only
	// where a send SQE is armed (prepSendSQE and the two non-ZC arming sites
	// in flushSend / flushSendLink); at most one send SQE is in flight per
	// connection — flushSend/flushSendLink both bail on cs.sending ||
	// cs.zcNotifPending — so one flag is exact. Worker-thread-only.
	sendIsZC    bool   // 1: the in-flight send SQE was armed as SEND_ZC
	zcSentBytes int32  // bytes sent from first SEND_ZC CQE (processed on NOTIF)
	sendBuf     []byte // 24: in-flight buffer (accessed with sending flag)

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

	// recvStallSince is the nanosecond timestamp at which this connection
	// entered a recv-arming stall: needsRecv set (an arm was dropped on a
	// full SQ ring), not paused, no recv in the kernel, and the dirty-list
	// retry declining to act because a SEND is still outstanding. Zero
	// when no stall is open. Worker-thread only — every read and write
	// sits on the event loop, next to cs.needsRecv itself. It exists only
	// to time the episode for the celeris#607 witnesses; the arming
	// decision never reads it.
	recvStallSince int64

	// lastRecvCQE / stallReported / pausesApplied back the silence
	// watchdog (CELERIS_DEBUG_RECV_STALL). lastRecvCQE is stamped on every
	// recv completion, stallReported keeps the watchdog to one record per
	// connection, and pausesApplied counts the backpressure pauses the
	// worker actually applied to it. All worker-thread-only, and all
	// written only while the probe gate is on.
	// linkArmedAt is when flushSendLink chained a RECV behind a SEND on
	// this connection, or 0 when no chain is outstanding. Worker-thread
	// only. The kernel will not start that recv until the send completes,
	// so the interval from here to the send's completion is time the
	// connection provably could not receive (celeris#607).
	linkArmedAt int64

	lastRecvCQE   int64
	stallReported bool
	pausesApplied uint32

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
	// recvCancelPending counts the backpressure pause's ASYNC_CANCELs whose
	// effect has not been observed yet: incremented when the pause submits
	// one, decremented by the -ECANCELED of a recv it cancelled or by the
	// cancel's own completion when it turns out to have cancelled nothing.
	// Non-zero tells handleRecv that a -ECANCELED recv CQE is one this
	// worker asked for, so a pause the middleware withdraws before the
	// cancel lands re-arms the connection instead of closing a healthy one.
	//
	// A COUNT, not a flag, because more than one can be outstanding: a conn
	// that pauses, resumes and pauses again before the ring is drained has
	// two cancels in flight, and they can resolve in either order. As a bool,
	// the first one to resolve as a MISS cleared the state the second — which
	// HIT — still needed, and its -ECANCELED then fell through handleRecv's
	// generic negative-result path and closed a healthy connection
	// mid-stream: the celeris#484 failure, reintroduced. Measured once in 12
	// oracle runs at MaxBackpressureBuffer=8 while celeris#596 was being
	// fixed with a bool. uint16 rather than uint8 so a burst of pause cycles
	// cannot wrap the count.
	//
	// Worker-thread only, like recvPaused.
	recvCancelPending uint16

	// transplantReap counts the hand-off's reported recv cancels (REAP,
	// celeris#657) whose effect has not been observed yet: incremented when
	// startReap submits one, decremented by the -ECANCELED of the recv one
	// of them cancelled or by a cancel's own completion reporting that it
	// matched nothing. A COUNT, for the reason recvCancelPending is one: a
	// reap can be outstanding for a recv that has since completed while a
	// newer reap targets the recv armed after it, and the older one's miss
	// must not clear the state the newer one's -ECANCELED needs — as a bool
	// it did, and that -ECANCELED fell through handleRecv's generic error
	// branch and closed a healthy connection (celeris#484/#596).
	//
	// reapStale: every reap counted in transplantReap was aimed at a recv
	// that has since completed, so the recv armed now has none aimed at it
	// and may get its own.
	//
	// transplantHold: the conn's last response was flushed with NO recv
	// behind it, because a drain was set and the hand-off at that SEND's
	// completion was expected to take the conn (HOLD). releaseHold arms the
	// recv there if the hand-off does not happen.
	//
	// reapSuppressed: the conn's last hand-off failed at handOff (the dup or
	// its non-blocking switch, e.g. EMFILE), so no reap is placed for it
	// until it next receives data. Without it, the recv re-armed after the
	// failure was reaped again at once and the hand-off failed again: a
	// RECV, a cancel and two completions per loop iteration for as long as
	// the failure and the drain both lasted.
	//
	// Only the conn's next data (handleRecv) and its release clear it, not
	// the end of the drain it was set in or the start of the next, so it
	// can outlive that drain (celeris#681 R5). Across drains the effect is
	// placement only, and narrow: a conn that has received nothing since
	// its dup failed meets a hand-off attempt in a later drain only at a
	// completion that is not data, for example a provided-buffer recv ended
	// by -ENOBUFS and re-armed, and no reap is placed for it there; its next
	// data clears the flag and the attempt after that data proceeds as
	// usual. A promoted async conn never has it set at a park: the data
	// that respawns its dispatch goroutine clears it first, so its hand-off
	// is retried at that park.
	//
	// All four worker-thread only, like recvCancelPending.
	transplantReap uint16
	reapStale      bool
	transplantHold bool
	reapSuppressed bool

	// Async handler dispatch (Worker.async=true, HTTP1 only):
	// Incoming recv bytes are appended under asyncInMu by the worker.
	// A single dispatch goroutine per conn drains asyncInBuf via a
	// double-buffer swap with asyncOutBuf, runs ProcessH1 under
	// detachMu, then enqueues on detachQueue so the worker submits
	// SEND SQEs on its own goroutine (SINGLE_ISSUER). The goroutine
	// parks on asyncCond between requests rather than exiting — saves
	// the ~1.5µs spawn cost on keep-alive conns. Shape matches
	// internal/engine/epoll's runAsyncHandler and preserves HTTP/1.1 pipelining.
	asyncInBuf  []byte
	asyncOutBuf []byte
	asyncInMu   sync.Mutex
	asyncCond   sync.Cond
	asyncRun    bool
	asyncClosed atomic.Bool
	// asyncParked (guarded by asyncInMu) is set while the dispatch goroutine
	// is in its park loop: waiting for input on asyncCond, or deciding at the
	// boundary whether to exit. It holds no detachMu there. dispatchBusy reads
	// it to tell a goroutine that may hold detachMu across a handler from one
	// that cannot (celeris#704).
	asyncParked bool
	// closeOwed (guarded by asyncInMu) is set by closeConn when it finds
	// detachMu held by this conn's RUNNING dispatch goroutine, i.e. held
	// across a user handler, and leaves the close to that goroutine instead
	// of parking the worker on the lock for the rest of the handler
	// (celeris#704). asyncClosed is already set, so the goroutine exits at its
	// next check, and the exit that finds closeOwed hands cs back through the
	// detach queue, whose asyncClosed branch runs closeConn again, on the
	// worker, with the lock free.
	closeOwed bool
	// relinkOwed (guarded by asyncInMu) is set by the dirty-list pass when it
	// gives the conn up because its dispatch goroutine holds detachMu across
	// a handler (celeris#704), and by a send completion held for the same
	// reason (heldSends, celeris#750). The goroutine hands cs back through
	// the detach queue at the top of its next loop, after the handler's own
	// flush, and drainDetachQueue applies the held completions and puts it
	// on the dirty list again.
	relinkOwed bool
	// heldSends (worker-thread only) are the ring SEND completions of this
	// conn that arrived while its dispatch goroutine held detachMu across a
	// handler, in arrival order (a SEND_ZC gives two). handleSend applies a
	// completion under detachMu, and waiting for the lock parked the worker,
	// and every connection of its ring, until the handler returned
	// (celeris#750, a fifth celeris#704 site). A completion cannot be dropped:
	// it is held, with relinkOwed set, and replayHeldSends applies it when the
	// goroutine hands the conn back, or when the conn is closed, before
	// anything else acts on the conn. Until then cs.sending (or
	// zcNotifPending) stays set, so no other SEND starts and every raw write
	// waits (celeris#751), and the dirty-list pass gives the conn up, which
	// it would spin on otherwise (flushDirty). The kernel's side of each is
	// done: kernelInflight was settled when it was dispatched.
	heldSends []completionEntry
	// closeErr (worker-thread only) is the error handleRecv's peer-FIN or
	// recv-error branch owes a detached middleware (OnError) when it met a
	// running handler holding detachMu. The branch used to deliver it under
	// the lock before closing; it now leaves it to closeConn, which delivers
	// it under the lock when the close actually runs (celeris#704).
	closeErr error
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
	// sweepKick is the drain epoch this conn's dispatch goroutine was last
	// Broadcast by the post-switch sweep (celeris#657 P9), so each promoted
	// async conn is woken at most once per drain. Worker thread, under
	// asyncInMu.
	sweepKick *transplantTargetHolder
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
	// detachCounted records that this conn actually contributed to
	// w.detachedCount, so the close path decrements only what was
	// incremented. Inferring it from h1State.Detached was wrong: in async
	// mode the dispatch goroutine sets Detached in OnDetach while the
	// increment is deferred to drainDetachQueue, so a close landing in that
	// window decremented for a conn that never counted (celeris#549).
	// Worker-thread only — both the increment sites and the decrement run
	// there.
	detachCounted bool

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
	//
	// recvArmSeq (validation builds only; zero-size in production, and not
	// the last field, so it adds no padding) is the SQ ring sequence number
	// of this conn's latest recv SQE: the close paths compare it with the
	// kernel's SQ head to tell a recv the kernel has not consumed yet
	// (celeris#715, Worker.recvUnsubmitted). Set by noteRecvPlaced.
	recvArmSeq     recvtheft.ArmSeq
	kernelInflight int32
	// recvArmed is true while a recv SQE (single-shot or multishot) is
	// kernel-held for this conn. Set by prepareRecv / flushSendLink's
	// linked recv, cleared when the recv's terminal CQE is dispatched.
	// The close paths use it to target an ASYNC_CANCEL at the armed
	// recv's exact generation-tagged user_data. Worker-thread-only.
	recvArmed bool
	// cancelMissed is set by a close path that queued cs for release without
	// having placed the cancel of a recv or send it left armed, because the
	// SQ ring had no room (cancelConnOps, celeris#869). The op is then owed
	// without anything having asked the kernel to end it: drainPendingRelease
	// places the cancel again, and its backstop holds cs for the op instead of
	// releasing it. Worker-thread-only; cleared at release.
	cancelMissed bool
	// recvOutstanding counts recv SQEs placed for this conn (prepareRecv,
	// flushSendLink's linked recv) minus terminal udRecv CQEs dispatched
	// to it. Mirrors recvArmed as a count so a second placement (2) and a
	// terminal CQE with nothing outstanding (0) are each observable —
	// see Worker.recvArm (celeris#586). Worker-thread-only.
	recvOutstanding int8
}

var connStatePool = sync.Pool{
	New: func() any {
		return &connState{
			writeBuf: make([]byte, 0, 4096),
			sendBuf:  make([]byte, 0, 4096),
		}
	},
}

// connGenSeq issues connection generations. It is process-monotonic rather
// than a per-connState counter, and that distinction is load-bearing.
//
// connStatePool is a sync.Pool, so GC drains it under connection churn and
// almost every acquire returns a FRESHLY ALLOCATED connState — for which
// `cs.generation++` yields 1, every time. Measured on the validation
// workload: 973/973 connections and 628/628 close-path cancels carried
// gen=1. The generation therefore provided ZERO disambiguation between
// successive occupants of the same fd, which is the one thing it exists to
// do (review 2.6).
//
// The consequence is celeris#470. cancelConnOps submits an ASYNC_CANCEL
// keyed on the recv's user_data (udRecv, fd, gen). If the fd is recycled
// before the kernel runs that cancel, the key matches the NEXT connection's
// recv byte-for-byte; staleConnCQE sees the generations agree, accepts the
// CQE as that connection's own, and handleRecv treats the resulting
// -ECANCELED as a fatal read error and closes a healthy connection without
// ever reading its request. On the wire: request ACKed, zero response
// bytes, FIN then RST, ~273us after SYN. Proven by connection identity --
// fd=130 live_cid=8255 killed by a cancel from cid=6241, 80us earlier.
//
// A process-monotonic source plus the 32-bit generation field makes a
// collision require 2^32 intervening accepts process-wide. At the measured
// end-to-end rate on this cluster (~32k accepts/s) that is ~37 hours, which
// no engine-side window can span -- and the binding window is NOT the
// microsecond cancel latency but the paths where no cancel is submitted at
// all: cancelConnOps skips the cancel when the SQ ring is full, leaving the
// op to the 5s pendingRelease backstop, and an armed udHeaderTimer lives for
// ReadHeaderTimeout (10s default). At 16 bits those windows spanned 2.4 and
// 4.9 wraps respectively, so a 16-bit generation would have left a real
// residual rather than closing the bug.
var connGenSeq atomic.Uint32

func acquireConnState(ctx context.Context, fd int, bufSize int, async bool) *connState {
	cs := connStatePool.Get().(*connState)
	cs.fd = fd
	// Draw a process-unique generation. gen==0 is skipped: encodeUserDataGen
	// collapses gen=0 onto the plain encodeUserData value, which would make a
	// conn-bound CQE indistinguishable from a non-conn-bound one.
	g := connGenSeq.Add(1)
	if g == 0 {
		g = connGenSeq.Add(1)
	}
	cs.generation = g
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
	cs.writeRefused = false
	cs.dirty = false
	cs.fixedFile = false
	cs.recvLinked = false
	cs.needsRecv = false
	cs.recvIntoBody = false
	cs.zcNotifPending = false
	cs.h2GoAwaySent = false
	cs.sendIsZC = false
	cs.zcSentBytes = 0
	cs.lastActivity = 0
	cs.recvStallSince = 0
	cs.linkArmedAt = 0
	cs.lastRecvCQE = 0
	cs.stallReported = false
	cs.pausesApplied = 0
	cs.detachMu = nil
	cs.detachClosed = false
	cs.recvPaused = false
	cs.recvPauseDesired.Store(false)
	cs.recvCancelPending = 0
	cs.transplantReap = 0
	cs.reapStale = false
	cs.transplantHold = false
	cs.reapSuppressed = false
	cs.headerTimerSpec = kernelTimespec{}
	cs.headerTimerArmed = false
	cs.forceRSTClose = false
	cs.asyncInBuf = cs.asyncInBuf[:0]
	cs.asyncOutBuf = cs.asyncOutBuf[:0]
	cs.asyncRun = false
	cs.asyncClosed.Store(false)
	cs.asyncParked = false
	cs.closeOwed = false
	cs.relinkOwed = false
	cs.heldSends = cs.heldSends[:0]
	cs.closeErr = nil
	cs.transplantPending.Store(false)
	cs.sweepKick = nil
	cs.asyncPromoted.Store(false)
	// asyncH2Promoted was missing here while every sibling was reset, so a
	// pooled connState came back still marked H2-promoted and was then
	// permanently ineligible for transplant (asyncTransplantEligible tests
	// it directly). celeris#544 — TestReleaseConnStateResetsEveryAtomicBool
	// enumerates these by reflection so the next field added cannot be
	// forgotten the same way.
	cs.asyncH2Promoted.Store(false)
	cs.promotedMethod = ""
	cs.promotedPath = ""
	cs.asyncDetachUnlocked = false
	cs.asyncDetachPending = false
	cs.detachCounted = false
	cs.bodyBuf = nil
	cs.sendBody = nil
	// bodyRecvPin is cleared here, after every kernel-held op delivered its
	// terminal CQE (releaseConnState is only called from drainPendingRelease
	// once cs.kernelInflight drained — or its backstop fired — and from the
	// backstop's SEND_ZC hold once a SEND_ZC, which writes nothing, is all
	// that is owed; releaseHeldConnState), so the kernel can no longer be
	// writing into the pinned bodyBuf array (#256 body-buffer UAF guard).
	cs.bodyRecvPin = nil
	cs.detectAccum = cs.detectAccum[:0]
	// kernelInflight is zero on every normal release (drainPendingRelease
	// gates on it); reset for the wall-clock-backstop path, where the worker
	// gave up waiting on a CQE the kernel never produced, and for the
	// backstop's SEND_ZC hold, which keeps the send buffer's array and lets
	// the connState go with the SEND_ZC still owed (releaseHeldConnState;
	// the identity's count, not this one, waits for its notification).
	cs.kernelInflight = 0
	cs.recvArmed = false
	cs.cancelMissed = false
	cs.recvOutstanding = 0
	cs.fd = 0
	cs.liveIdx = -1
	connStatePool.Put(cs)
}

// endDispatch marks cs's dispatch goroutine as gone and reports whether it
// owes the worker a hand-back: a close or a relink left to it (closeOwed,
// relinkOwed; celeris#704). The goroutine calls it on every exit path,
// holding cs.asyncInMu. A path that enqueues cs on its way out ignores the
// result: that enqueue is the hand-back, and drainDetachQueue settles both
// debts (the asyncClosed branch runs the close; any other entry puts the conn
// back on the dirty list). The paths that exit WITHOUT enqueuing must enqueue
// when it reports true, or the close or the recv arm it stands for is lost.
func (cs *connState) endDispatch() (owed bool) {
	cs.asyncRun = false
	cs.asyncParked = false
	owed = cs.closeOwed || cs.relinkOwed
	cs.closeOwed = false
	cs.relinkOwed = false
	return owed
}

// dispatchBusy reports whether cs's dispatch goroutine may be holding
// cs.detachMu across a user handler: it is alive (asyncRun), not in its park
// loop (asyncParked), and has not released the lock for good at a Detach
// (asyncDetachUnlocked). All three are read under asyncInMu. Worker thread.
//
// It is how a worker-thread site that found cs.detachMu held (TryLock failed)
// tells the holders apart (celeris#704, the io_uring twin of celeris#669). The
// dispatch goroutine holds the lock across ProcessH1, i.e. for as long as the
// handler runs, and it is running whenever it does. Every other holder, a
// detached conn's guarded writeFn, holds it for one write. So a site that
// finds the lock held while this reports true must not wait, and one that
// finds it held while this reports false may wait as it always has: that wait
// is bounded.
//
// After Detach the goroutine never takes the lock across ProcessH1 again, so
// it is excluded even while it runs: a handler may keep streaming after
// Detach, and a close left to it would wait for that handler, which in turn
// waits for the close's OnDetachClose to learn it should stop.
//
// If owe is non-nil and the result is true, *owe is set in the same critical
// section: the goroutine reads it under asyncInMu at the top of its next loop
// or at its exit, so it cannot miss it. A running goroutine is not
// necessarily the holder, and every caller acts on a true only by leaving
// work to the goroutine, which hands it back; a false positive costs a
// hand-back, never a lost close or flush.
func dispatchBusy(cs *connState, owe *bool) bool {
	if cs.asyncCond.L == nil {
		return false // no async machinery: sync mode, no dispatch goroutine
	}
	cs.asyncInMu.Lock()
	busy := cs.asyncRun && !cs.asyncParked && !cs.asyncDetachUnlocked
	if busy && owe != nil {
		*owe = true
	}
	cs.asyncInMu.Unlock()
	return busy
}
