//go:build linux

package iouring

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/ctxkit"
	"github.com/goceleris/celeris/internal/deferlinger"
	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/engine/internal/bindiag"
	"github.com/goceleris/celeris/internal/engine/internal/errclass"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/protocol/detect"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
	"github.com/goceleris/celeris/internal/recvtheft"
	"github.com/goceleris/celeris/internal/resource"
	"github.com/goceleris/celeris/internal/sockopts"
	"github.com/goceleris/celeris/internal/wakefd"
	"github.com/goceleris/celeris/internal/zcwindow"
	"github.com/goceleris/celeris/validation"

	"golang.org/x/sys/unix"
)

// fixedFileTableSize is the number of slots in the fixed file table.
// Must accommodate the maximum number of concurrent connections per worker.
const fixedFileTableSize = 65536

// errPeerClosed is what detached middleware is told when the peer performs an
// orderly shutdown. It wraps io.EOF because every consumer classifies a clean
// disconnect with errors.Is(err, io.EOF). Handing them the recv result instead
// produced unix.Errno(0) — printed as "errno 0" — which matches nothing, so an
// ordinary disconnect was scored as a protocol error (celeris#564). epoll
// reports the same condition through its own errPeerClosed.
var errPeerClosed = fmt.Errorf("celeris: peer closed connection: %w", io.EOF)

// errWriteRefused ends an async dispatch goroutine whose handler's writes
// the back-pressure cap refused (celeris#761): the worker closes the conn.
var errWriteRefused = errors.New("celeris: write refused: send backlog over the limit")

// errIORingRecv wraps a negative io_uring recv result as a syscall.Errno.
// Used to surface concrete errors to detached middleware via H1State.OnError.
// Callers MUST handle res == 0 before reaching here: a zero result is an
// orderly shutdown, not a failure, and unix.Errno(0) is not a usable error.
func errIORingRecv(res int32) error {
	return unix.Errno(uint32(-res))
}

// errIORingSend wraps a negative io_uring send result as a syscall.Errno.
func errIORingSend(res int32) error {
	return unix.Errno(uint32(-res))
}

// bufRingGroupID is the provided buffer ring group ID.
const bufRingGroupID = 0

// bufRingCountMin is the floor for the scaled provided-buffer-ring size.
// Below ~1024 entries the kernel reports ENOBUFS under 1500+ conn churn
// (the per-conn re-arm path fires constantly and burns the latency
// budget). The formula below keeps this as the floor and scales up from
// there. See resolveBufRingCount for the full rationale.
const bufRingCountMin = 1024

// bufRingCountMax caps the scaled provided-buffer-ring size. The hard
// ceiling is the kernel's IORING_REGISTER_PBUF_RING limit of 32768
// entries (a ring larger than that is rejected by the kernel) — which is
// also the largest count the uint16 tail/mask/bid arithmetic in
// BufferRing can address. At the default 8 KiB BufferSize this is 256 MiB
// of buffer memory per worker at the absolute max; the auto-scaling
// formula stays far below this in practice. Operators with unusual
// workloads can override via the env var, but never past this cap.
const bufRingCountMax = 1 << 15 // 32768 entries × 8 KiB = 256 MiB worst case (kernel PBUF_RING cap)

// CELERIS_IOURING_PBUF_COUNT overrides the auto-scaled provided-buffer-ring
// size. A value that is not a power of 2 is rounded up to one, and the result
// is clamped to [bufRingCountMin, bufRingCountMax]. Use this when
// the default scaling formula under-provisions your workload — typically
// the case for very-high-concurrency benchmarks (16k+ connections) where
// each worker may have more in-flight multishot recvs than the formula
// anticipates. Setting 0 (or leaving the env var unset) reverts to
// auto-scaling from the per-worker conn target.
const envPbufCount = "CELERIS_IOURING_PBUF_COUNT"

// envFixedFiles opts into the registered-file-table path. Development only:
// the feature is incomplete (celeris#541) and the default single-shot recv
// omits IOSQE_FIXED_FILE, so enabling it makes connections read from
// unrelated descriptors.
const envFixedFiles = "CELERIS_IOURING_FIXED_FILES"

// fixedFilesEnabled reports whether the registered-file path should actually
// be used: the tier must support it AND it must be explicitly opted into.
//
// Kept as one function so the worker's gate and the engine's startup log
// cannot disagree. They did: the log reported the tier CAPABILITY, so it
// printed fixed_files=true while the feature was off.
func fixedFilesEnabled(tierSupports bool) bool {
	return tierSupports && os.Getenv(envFixedFiles) == "1"
}

// defaultConnsPerWorker is the per-worker connection target used to size
// the provided-buffer ring. The ring is sized at 2 buffers per conn at
// this target, giving comfortable headroom so the kernel rarely stalls
// waiting for buffer returns. Operators override the resulting ring size
// directly via CELERIS_IOURING_PBUF_COUNT.
const defaultConnsPerWorker = 20

// resolveBufRingCount picks the provided-buffer-ring size for a worker.
// The default formula is `nextPowerOf2(max(bufRingCountMin, 2 *
// connsPerWorker))`, i.e. two buffers per conn at the per-worker conn
// target — enough headroom that the kernel rarely stalls waiting for
// buffer returns. Above 1024 conns the previous hard-coded 1024 was too
// small: buffers were reused aggressively, the kernel stalled, and the
// very behaviour the ring is designed to optimise (multishot recv CQE
// batching) collapsed into CQE storms (celeris#322).
//
// The ring is PER-WORKER: NewBufferRing is created once per Worker on its
// own ring, so the scaling MUST be driven by the per-worker conn target,
// NOT by the engine-wide Workers count. Multiplying by Workers over-sized
// every worker's ring by the worker count, wasting count×BufferSize of
// mmap'd RSS per worker and risking the kernel cap on large boxes
// (celeris#322 follow-up).
// Operators can override via CELERIS_IOURING_PBUF_COUNT.
func resolveBufRingCount(_ resource.ResolvedResources, connsPerWorker int) int {
	if v := os.Getenv(envPbufCount); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n&(n-1) != 0 {
				n = resource.NextPowerOf2(n)
			}
			return clampBufRingCount(n)
		}
	}
	target := connsPerWorker
	if target <= 0 {
		target = defaultConnsPerWorker
	}
	scaled := 2 * target
	if scaled < bufRingCountMin {
		scaled = bufRingCountMin
	}
	return clampBufRingCount(resource.NextPowerOf2(scaled))
}

func clampBufRingCount(n int) int {
	if n < bufRingCountMin {
		return bufRingCountMin
	}
	if n > bufRingCountMax {
		return bufRingCountMax
	}
	return n
}

// pendingReleaseEntry queues a connState for deferred release — its
// fd has been closed but the kernel may still have SQEs referencing
// its buffers; we hold it until cs.kernelInflight reports every
// kernel-held op has produced its terminal CQE (the close path submits
// ASYNC_CANCELs so this is normally within a loop pass or two), with
// releaseAtNanos as the wall-clock BACKSTOP for kernel anomalies. See
// Worker.pendingRelease docstring.
//
// detached entries (async-dispatch / WS / SSE conns) skip the pool
// recycle: their cs has live state (h1State, asyncCond, asyncInBuf
// slices) that goroutine closures may still reference even after the
// worker observes asyncClosed and runs finishCloseDetached. Holding
// the strong ref alive past the kernel's recv-SQE drain window is
// what we need; the recycle path resetting fields would race with the
// goroutine's defer that reads cs.fd / cs.asyncInBuf. (The goroutine's
// own closure references remain visible to GC after we drop ours, so
// dropping the ref once the kernel ops drained is safe.)
//
// holdsFD marks an entry whose close path left its descriptor, fd, OPEN
// because the kernel still owed an op on it (celeris#685, see closeFDOwed);
// drainPendingRelease closes it at the same point it releases cs: when the
// last owed op has delivered its terminal CQE. A flag rather than an fd of -1
// so that the zero entry holds nothing. Both sit in detached's padding, so the
// entry stays 24 bytes on 64-bit platforms
// (TestPendingReleaseEntryStaysTwentyFourBytes).
//
// zcHeld marks an entry the backstop found still owing a SEND_ZC and held
// (celeris#812, see closedZCOwed and holdZCPastBackstop): the kernel may still
// read cs.sendBuf, so its array is held, off this queue, until that op's
// notification arrives, however long past releaseAtNanos. It records only
// that the hold was counted (CloseZCNotifHeld), once per entry, for an entry
// that comes back to the queue and is held again; the same padding holds it.
type pendingReleaseEntry struct {
	cs             *connState
	releaseAtNanos int64
	detached       bool
	holdsFD        bool
	zcHeld         bool
	// cancelHeld records that the backstop held the entry for a cancel that
	// was never placed (cs.cancelMissed) and counted it, once.
	cancelHeld bool
	fd         int32
}

// closedOpsEntry is the Worker.closedOps value: the conn(s) closed under
// one (fd, generation) user_data identity and the total number of
// terminal recv/send CQEs the kernel still owes them. conns has one
// element except under an fd+generation collision, where the CQEs are
// indistinguishable and every colliding conn is held until the combined
// count drains (release-late is safe; release-early is the UAF).
type closedOpsEntry struct {
	inflight int32
	// handoff marks an identity a conn left through a transplant hand-off
	// rather than a close (noteHandedOffInflight), so a stale recv CQE that
	// carried data for it is counted as a hand-off loss (celeris#657). It
	// is read only by that counter and changes nothing else. It sits in
	// inflight's padding, so on 64-bit platforms the entry stays 32 bytes
	// (TestClosedOpsEntryStaysThirtyTwoBytes). 32-bit platforms have no
	// padding there, and the entry grows from 16 to 20 bytes.
	handoff bool
	// zcOwed is the part of inflight that is a SEND_ZC (celeris#812): the
	// send, and after its first CQE its notification. The kernel may read
	// the conn's sendBuf until that notification. noteClosedInflight adds
	// one per conn whose send in flight is a SEND_ZC, and
	// noteStaleTerminalOp takes it off at that op's terminal CQE (see
	// there). closedZCOwed reads it, so the release backstop holds the
	// connState, and the send buffer with it, until the kernel is done with
	// the buffer. A conn has one send in flight at most, so a byte is ample
	// even under a collision; it takes the byte of padding between handoff
	// and fdOps, and the entry keeps its size on every platform.
	zcOwed uint8
	// fdOps is the part of inflight that still names the descriptor
	// (celeris#798): all of it but the SEND_ZC notifications whose send has
	// completed. noteClosedInflight adds each conn's fdOps; staleConnCQE
	// takes one off at a recv's or a plain send's terminal CQE and at a
	// SEND_ZC's first CQE, and none at its notification. closedFDNamed
	// reads it, so a close that kept its descriptor lets go of it once only
	// notifications are owed, while the connState waits for inflight. A
	// conn owes two such ops at most (a recv and a send), so int16 is ample
	// even under a collision; it sits in the same padding as handoff, and
	// the entry keeps its size on every platform.
	fdOps int16
	conns []*connState
}

// pendingReleaseHoldNanos is the WALL-CLOCK BACKSTOP for releasing a
// closed connState whose kernel ops never produced a terminal CQE.
//
// It is NOT the primary release gate. Release is gated on
// cs.kernelInflight == 0: the close path ASYNC_CANCELs the armed
// recv/send by generation-tagged user_data and drainPendingRelease
// frees the connState only once every cancelled/completed op has
// delivered its terminal CQE. That is exact — no window to
// tune. The wall clock only matters if the kernel never delivers a
// terminal CQE at all (or the cancel SQE was dropped on a full SQ
// ring); when it fires, drainPendingRelease logs a WARN because a
// kernel-held op may still reference cs.buf and releasing is a
// last-resort trade of a potential use-after-free against an
// unbounded memory leak.
//
// One op is exempt, because for it the trade is not a last resort: a
// SEND_ZC, whose terminal CQE is its notification (celeris#812). The
// kernel posts it once no queued segment references the send buffer's
// pages any more, and a peer that stopped reading keeps the unsent tail
// queued on the socket for as long as the socket lives: after a close,
// until the kernel gives up on the orphaned socket, minutes on a peer that
// keeps acknowledging its closed window. Until then the kernel reads
// cs.sendBuf whenever the peer opens its window, so a release there was a
// certain use-after-free, not a potential one: the peer received whatever
// the array's next owner wrote into it. The backstop holds such a
// send buffer until the notification arrives instead (closedZCOwed,
// holdZCPastBackstop). Nothing but the peer decides how long that is: a peer
// that keeps reading, however slowly, keeps the orphaned socket alive. So the
// hold is kept off this queue's walk, keeps the array alone once the SEND_ZC
// is all that is owed, shows in the CloseZCNotifHeldNow/Bytes gauges, and
// stops the worker arming new SEND_ZCs while it holds zcHoldBytesMax (see
// zc_send_buffer.go). It never costs a descriptor.
//
// 5 s is comfortably above any plausible straggler window: TCP
// retransmits begin at RTO_MIN = 200 ms and a 4 KiB POST tail
// straggling through several backoffs stays well under a second —
// the 100 ms hold this replaces sat BELOW RTO_MIN, which is exactly
// how the v1.4.15/7beebb9 heap corruption slipped past it.
//
// The backstop MUST stay time-based, not iteration-based: under
// churn-close the loop spins sub-millisecond per iteration while idle
// iterations stretch to 100 ms+ (see the v1.5.0 review 2.9 history on
// queuePendingRelease).
const pendingReleaseHoldNanos int64 = int64(5 * time.Second)

// shutdownSendDrainNanos is the floor of the send drain the run loop performs
// after its context is cancelled (celeris#595): responses prepared by
// handlers that were still running when Server.Shutdown fired are submitted
// and completed through the normal loop rather than being discarded with the
// fd. 250 ms is far above a loopback/LAN send completion, and it is all the
// drain gets when no live budget was handed over: a peer that has stopped
// reading simply hits it. While the budget of the last Engine.Shutdown is
// live the drain lasts as long as that budget, as epoll's send drain does
// (drainEnd, celeris#806).
const shutdownSendDrainNanos int64 = int64(250 * time.Millisecond)

// waitUntimed reports whether the run loop may wait for a completion with no
// timeout (mode 3a): SEND SQEs are being submitted, each of which produces a
// CQE, and the worker is not draining. A draining worker (its context is
// cancelled) never does: a SEND to a peer that does not read produces none, and
// the end of the drain is looked at only between waits (celeris#806). Small
// enough to inline; a function so the rule can be tested.
func waitUntimed(hasPending, sendsPending, draining bool) bool {
	return hasPending && sendsPending && !draining
}

// closingDrainTimeoutNanos bounds the deferred-close drain: how long a
// connection may sit with cs.closing set, waiting for the SENDs queued at
// close time to reach the kernel, before checkTimeouts tears it down anyway.
//
// The drain had no other bound. closeConn defers the fd close so the last
// bytes (GOAWAY / RST_STREAM / WS close-echo) reach the client, and its only
// exit is the SEND's own CQE via completeSend. A peer that stops reading — a
// killed SSE client whose socket lingers, a WS client behind a wedged proxy —
// never lets that SEND complete, and the timeout sweep skips cs.closing conns,
// so the fd, the connState and the activeConns slot were held until the peer
// eventually disconnected (celeris#498).
//
// 5 s, matching pendingReleaseHoldNanos: both are last-resort wall-clock
// backstops for an operation the kernel may never complete, and keeping them
// on one scale means a wedged conn is fully reclaimed — fd here, connState
// when the close-path ASYNC_CANCEL's terminal CQE lands — inside one such
// window rather than two unrelated ones. It is the floor of the bound, not
// the bound: closingDrainBound extends it to cfg.WriteTimeout, since the
// queue of a closing conn can be a whole response (celeris#761). Either way
// it is not a throughput budget but the point at which we conclude the peer
// will never take the bytes: the clock restarts whenever it takes some. A
// closing conn's queue is final (writeFn no-ops once detachClosed is set,
// drainDetachQueue skips it, and handleRecv drops incoming data on a closing
// conn).
//
// Prompt completions are untouched: the SEND CQE normally lands within a loop
// pass or two — microseconds on localhost, four orders of magnitude inside
// this window — so a conn whose sends drain still closes via completeSend's
// path and never reaches the sweep.
const closingDrainTimeoutNanos int64 = int64(5 * time.Second)

// closingDrainBound is how long a closing conn may go without the peer
// taking a byte of what is queued before checkTimeouts tears it down: the
// longer of closingDrainTimeoutNanos and cfg.WriteTimeout, the bound a live
// conn stalled on a write gets. completeSend restamps the clock on every send
// that makes progress. A closing conn may carry a whole response, not only
// its last bytes: a response larger than the socket buffers answering
// Connection: close, or followed by a request error. A client that paused
// reading it for more than 5 s, where it had WriteTimeout on a keep-alive
// conn, lost its tail (celeris#761).
func (w *Worker) closingDrainBound() int64 {
	return max(closingDrainTimeoutNanos, int64(w.cfg.WriteTimeout))
}

// Worker is an io_uring event-loop worker pinned to a single OS thread.
type Worker struct {
	id         int
	cpuID      int
	ring       *Ring
	listenFD   int
	tier       TierStrategy
	fixedFiles bool // runtime flag: true if ACCEPT_DIRECT is working
	sqpoll     bool // true when SQPOLL is active (kernel submits SQEs)
	sendZC     bool // true when SEND_ZC is available (kernel 6.0+)
	async      bool // true when Config.AsyncHandlers dispatches handlers to spawned Gs
	// h1Only is true when engine config locks every conn to HTTP/1.1
	// (Protocol == HTTP1 AND EnableH2Upgrade == false). cs.protocol is set
	// once at registerConn and never written, so the recv hot path can
	// skip the atomic Load.
	h1Only bool

	// asyncWG tracks runAsyncHandler goroutines so graceful shutdown
	// can Wait on them before returning. See internal/engine/epoll/loop.go
	// for rationale — keeps dispatch Gs from touching connState
	// memory after the engine claims to have stopped.
	asyncWG   sync.WaitGroup
	conns     []*connState
	connCount int // number of active connections (local, for draining check)
	maxFD     int // upper bound fd for iteration in checkTimeouts/shutdown
	// liveConns is a dense slice of currently-active FDs, maintained
	// alongside the sparse conns map. checkTimeouts and shutdown iterate
	// liveConns to avoid the O(maxFD) scan that dominated the worker
	// hot path above ~8 Ki conns (celeris#318). Append-on-register,
	// swap-with-last-on-deregister; the slice is owned by the worker
	// thread so no locking is required.
	liveConns []int
	// The post-switch sweep (celeris#657 P9, sweep.go). Worker-thread-only
	// but sweepCnt, which points at the engine-wide gauges every worker
	// publishes into.
	// sweepArrived records that a connection JOINED the live set during the
	// cycle in progress, which is appended past the cursor and so cannot be
	// reached by it: such a cycle may not go dormant (THE CYCLE RULE in
	// internal/engine/epoll/sweep.go, celeris#657 R2).
	sweepH         *transplantTargetHolder
	sweepCnt       *sweepCounters
	sweepNext      int64
	sweepIvl       int64
	sweepCursor    int
	sweepDormant   bool
	sweepArrived   bool
	cycleMoved     int
	cycleTransient int
	cycleRes       [numResidual]uint64
	sweepPub       [numResidual]uint64
	handler        stream.Handler
	resolved       resource.ResolvedResources
	sockOpts       sockopts.Options
	runCtx         context.Context //nolint:containedctx // stored so #383 transplant attach (off the accept path) can derive a conn ctx
	bufRing        *BufferRing     // ring-mapped provided buffers for multishot recv
	logger         *slog.Logger
	cfg            resource.Config
	ready          chan error
	acceptPaused   *atomic.Bool
	// acceptRearmPending is set when prepareAccept could not place the
	// accept SQE because the SQ ring was full. In multishot mode the
	// listen socket has exactly one accept SQE in flight, re-armed only
	// from handleAccept when a CQE arrives without F_MORE; if that re-arm
	// is dropped nothing else ever arms accept again and this worker's
	// SO_REUSEPORT listen socket goes deaf while the kernel keeps
	// completing TCP handshakes into its backlog. The event loop retries
	// the arm at the top of every iteration until it lands. Loop-thread
	// only, like listenFD.
	acceptRearmPending bool

	// h2PollRearmPending is set when prepareH2Poll could not place its
	// POLL_ADD because the SQ ring was full. The poll is SINGLE-SHOT and
	// nothing anywhere clears h2PollArmed, so a dropped arm leaves the
	// eventfd permanently deaf: no later caller re-arms it, and the detach
	// queue's wakeup write is coalesced on the empty -> non-empty edge, so
	// no later enqueue writes the fd either. A worker with no other traffic
	// then sleeps in SubmitAndWait with queued work it will never see.
	// Retried at the top of the loop, like the accept arm.
	h2PollRearmPending bool
	wake               chan struct{}
	wakeMu             sync.Mutex
	suspended          atomic.Bool
	// listenFDClosed signals that the worker has no listen FD while
	// acceptPaused is set: its pause linger has run out, it has cancelled
	// its in-flight accept SQE and closed the listener, or it has exited
	// (shutdown sets it). PauseAccept waits on this so it only returns once
	// the SO_REUSEPORT group has actually shed this listener; the worker
	// notifies the wait (pause.Notify) after it sets the flag. It stays
	// false while the worker lingers (celeris#662): the listener is still
	// open and its accept still armed then.
	listenFDClosed atomic.Bool
	// lingerUntil is the deadline, on deferlinger's monotonic clock, of the
	// accept-pause linger in progress, or 0 when none is (celeris#662): the
	// listener's TCP_DEFER_ACCEPT has been cleared and its multishot accept
	// stays armed until then. Loop-thread only, like listenFD.
	lingerUntil deferlinger.Deadline
	// deferCapable records whether listenFD was created with
	// TCP_DEFER_ACCEPT, which is whether a pause has anything to clear and
	// linger for. A listener created without it (DisableDeferAccept) closes
	// at once. Loop-thread only.
	deferCapable bool
	// pause is the engine's record of the pause in progress, written by
	// BeginPauseAccept before it sets acceptPaused. Nil in bare test
	// workers, which deferlinger treats as "no guard, no delay".
	pause *deferlinger.PauseState

	reqCount    *atomic.Uint64
	activeConns *atomic.Int64
	// errs is the engine-wide per-cause ErrorCount breakdown, shared by
	// every worker (celeris#645). There is no aggregate counter beside it:
	// EngineMetrics.ErrorCount is the sum of these buckets.
	errs            *errclass.Counters
	asyncPromoted   *atomic.Uint64 // cumulative inline → dispatch promotions (#300)
	acceptCount     *atomic.Uint64 // cumulative accepts (engine-wide, shared)
	closeCount      *atomic.Uint64 // cumulative closes (engine-wide, shared)
	bytesRead       *atomic.Uint64 // cumulative recv payload bytes (engine-wide, shared)
	bytesWritten    *atomic.Uint64 // cumulative send payload bytes (engine-wide, shared)
	transplantCount *atomic.Uint64 // cumulative #383 adopt-from-other-engine count (engine-wide, shared; nil-safe)
	// The rest of the #383 hand-off ledger (celeris#624), all engine-wide,
	// shared and nil-safe so a bare test Worker literal can skip them.
	transplantDetached     *atomic.Uint64 // conns detached FOR epoll (no OnDisconnect fired)
	transplantSlotOccupied *atomic.Uint64 // adoptions refused on an occupied slot (fd not closed)
	closeMissingConnState  *atomic.Uint64 // finishClose on a nil connState (OnDisconnect skipped)
	// One bucket per remaining silent drop point on the hand-off path
	// (celeris#624).
	transplantHandoffRefused *atomic.Uint64 // target refused an already-relinquished fd
	transplantAdoptRefused   *atomic.Uint64 // adoption refused for a reason other than a taken slot
	// handoffLoss is the engine-wide celeris#657 witness set: stale recv
	// data by identity class and hand-offs made with an op in flight.
	// nil-safe so a bare test Worker literal can skip it.
	handoffLoss *handoffLossStats
	// recvArm is the engine-wide recv-arming witness set (celeris#586);
	// nil-safe so a bare test Worker literal can skip it.
	recvArm *recvArmStats
	// zc is the engine-wide SEND_ZC exposure witness set (celeris#591);
	// nil-safe so a bare test Worker literal can skip it.
	zc                *zcStats
	reqBatch          uint64 // batched request count, flushed to reqCount per iteration
	bytesReadBatch    uint64 // batched recv bytes, flushed to bytesRead per iteration
	bytesWrittenBatch uint64 // batched send bytes, flushed to bytesWritten per iteration
	// ringBytesBatch is the ring-send share of bytesWrittenBatch (the
	// complement of the inline-egress bytes counted at the write site),
	// flushed to zc.ringBytes on the same per-iteration cadence. Batched
	// rather than an atomic add because completeSend IS the per-request
	// send path (celeris#591).
	ringBytesBatch uint64
	// linkArmBatch is the batched count of SEND→RECV chains armed by
	// flushSendLink, flushed to recvArm.linkedRecvArms once per event-loop
	// iteration alongside reqBatch. flushSendLink IS the per-request send
	// path, so the witness must not put an atomic on it (celeris#607).
	linkArmBatch uint64

	tickCounter uint32
	cachedNow   int64  // cached time.Now().UnixNano(), refreshed on every CQE-bearing iteration and by checkTimeouts
	iterCount   uint64 // monotonic event-loop iteration counter (for pendingRelease)

	// pendingRelease defers returning connState structs to the pool
	// until the kernel has drained every in-flight I/O SQE that may
	// still hold pointers into cs.buf / cs.sendBuf. unix.Close(fd)
	// does NOT complete a pending io_uring recv (the op holds its own
	// file reference — only inbound data, FIN, or an ASYNC_CANCEL
	// completes it), so the close path cancels the armed ops and this
	// queue holds cs until cs.kernelInflight confirms their terminal
	// CQEs arrived. If we released cs earlier, Go's GC could reclaim
	// cs.buf's backing array; the kernel then writes straggler bytes
	// (e.g. a retransmitted POST segment at RTO ≥ 200 ms) into memory
	// the runtime has repurposed — historically a SIGSEGV in
	// runtime.stackalloc dereferencing HTTP bytes as a pointer (#256),
	// and on Go 1.26 (Green Tea GC, in-span alloc/mark bits) a fatal
	// "s.allocCount != s.nelems" span-corruption (v1.4.15/7beebb9, both bench runs). Neither is
	// race-detectable because the writer is the kernel, not Go code.
	// releaseAtNanos is only the anomaly backstop — see
	// pendingReleaseHoldNanos, which also says why it never gives up a
	// send buffer a SEND_ZC may still read (celeris#812), and holds it off
	// this queue instead (zcHolds).
	pendingRelease []pendingReleaseEntry
	// cancelSQEFull, when set (tests only), makes getCancelSQE report a full
	// SQ ring while it returns true: the case it has to answer with no SQE,
	// which a ring with room, and a kernel that takes every submit, never
	// produces (celeris#869).
	cancelSQEFull func() bool
	// closeFDOwed counts the pendingRelease entries that still hold their
	// descriptor open (holdsFD, celeris#685). The worker does not park
	// while it is non-zero: the terminal CQEs those closes wait for arrive
	// only through the ring, and a parked worker enters no ring. Worker
	// thread only. closeFDDeferredBatch is the count of such closes not yet
	// added to handoffLoss.closeFDDeferred, flushed once per loop iteration.
	closeFDOwed          int
	closeFDDeferredBatch uint64

	// closedOps routes terminal recv/send CQEs that arrive AFTER their
	// conn was closed (w.conns[fd] already nil or reused) back to the
	// closed connState for kernelInflight accounting. Keyed by
	// connOpKey (generation<<40 | fd) — the op tag is stripped so one
	// entry covers a conn's recv and send. Populated by
	// noteClosedInflight on the close paths, consumed by
	// noteStaleTerminalOp at CQE dispatch; nil/empty whenever no
	// closed conn has kernel ops outstanding, so the hot path pays a
	// single len check. Worker-thread-only.
	//
	// A (fd, generation) collision — two closed conns whose ops share
	// a user_data identity, possible under fd reuse because generations
	// are per-connState-object — makes their terminal CQEs mutually
	// indistinguishable, so the entry carries a COMBINED count and
	// releases all colliding conns only after every expected terminal
	// CQE arrived (errs toward holding longer; see closedOpsEntry).
	closedOps map[uint64]*closedOpsEntry
	// zcHolds are the send buffers the pendingRelease backstop holds past
	// its deadline for a SEND_ZC still owed on them (celeris#812), keyed by
	// the closed identity (connOpKey) whose CQEs settle them
	// (settleZCHolds, from noteStaleTerminalOp). Kept off pendingRelease,
	// which the loop walks every pass, because a hold lasts as long as the
	// peer keeps its orphaned socket alive (see zc_send_buffer.go).
	// zcHoldCount and zcHoldBytes are what they hold: the worker's share of
	// the held-now gauges, and zcHoldBytes is what prepSendSQE checks
	// against zcHoldBytesMax. Worker thread only; nil/zero whenever nothing
	// is held.
	zcHolds     map[uint64][]zcHold
	zcHoldCount int
	zcHoldBytes int

	// shutdownDrainStart is when (UnixNano) the send drain the run loop
	// performs once its context is cancelled began (celeris#595): after the
	// HTTP/2 pool handlers are done (h2PoolSettled), so zero until then. The
	// drain's end is drainEnd(shutdownDrainStart, ...), worked out afresh
	// every iteration, since the budget it follows can arrive after the
	// cancel (Engine.Shutdown follows the cancel of StartWithContext's
	// context). draining is true from the first iteration that finds the
	// context cancelled: no wait in the loop may then be unbounded
	// (celeris#806). Worker thread only; draining is read on the hot path,
	// in the one branch that waits without a timeout and in adaptiveTimeout.
	shutdownDrainStart int64
	draining           bool

	// refuseRecv is true once the send drain is past its first
	// shutdownSendDrainNanos: handleRecv then reads nothing more from an
	// HTTP/1 connection (celeris#806). The drain lasts as long as the
	// budget, and on keep-alive connections whose responses queue in the
	// worker, new requests would keep the queues from ever emptying.
	// Worker thread only; read in handleRecv.
	refuseRecv bool

	// drainBudget points at the engine's record of the budget of the last
	// Engine.Shutdown call, and h2DrainStart is when the worker, its
	// context cancelled, began waiting for the HTTP/2 stream handlers on
	// the shared worker pool (celeris#759; h2PoolSettled). Worker thread
	// only; h2DrainStart is zero until then.
	drainBudget  *atomic.Pointer[context.Context]
	h2DrainStart int64

	dirtyHead     *connState // head of intrusive doubly-linked dirty list
	hasBufReturns bool       // set when provided buffers need publishing
	sendsPending  bool       // true when SEND SQEs are in the SQ ring (guarantees CQE production)
	h2Conns       []int      // FDs of H2 connections (for write queue polling)
	// wakeFD owns the H2 / wakeup eventfd. Producers on other goroutines
	// (drivers, transplants, detached WS/SSE, the H2 write queue) signal
	// through the handle, so none of them can write the descriptor number
	// after shutdown closed it (celeris#655).
	wakeFD         *wakefd.WakeFD
	h2PollArmed    bool // true when POLL_ADD is active on the wakeup eventfd
	h2cfg          conn.H2Config
	emptyIters     uint32 // consecutive iterations with zero CQEs (for adaptive timeout)
	detachQueue    []*connState
	detachQMu      sync.Mutex
	detachQSpare   []*connState
	detachQPending atomic.Int32 // 1 when detachQueue has entries; gates the hot-path drain
	detachedCount  int          // number of currently-detached conns; gates idle-deadline sweep
	// detachedConns mirrors detachedCount into an engine-wide atomic so the
	// accounting is observable off the worker thread; detachWindowCloses
	// counts closes that land inside the celeris#549 window (Detach
	// published, deferred increment not yet taken). Engine-wide, shared,
	// nil-safe (hand-built Workers in unit tests leave them nil). See
	// celeris#584.
	detachedConns      *atomic.Int64
	detachWindowCloses *atomic.Uint64

	// EventLoopProvider state. driverConns is keyed by real FD and is
	// completely disjoint from the HTTP conns array. hasDriverConns is the
	// zero-cost gate: when false, the HTTP fast path pays no overhead.
	driverConns         map[int]*driverConn
	driverMu            sync.RWMutex
	hasDriverConns      atomic.Bool
	driverActionQueue   []driverAction
	driverActionSpare   []driverAction
	driverActionMu      sync.Mutex
	driverActionPending atomic.Int32
	// adoptClosed is set under driverActionMu by closeAdoptQueue when the
	// worker shuts down; AdoptConn refuses from then on (celeris#658).
	adoptClosed bool
	// driversClosed is set under driverMu by shutdownDrivers; RegisterConn
	// refuses from then on (celeris#691). shutdownDrivers runs once, so a
	// conn registered after it would never be retired, and the duplicate
	// descriptor RegisterConn takes for it would never be closed.
	driversClosed bool
	// driverClosers counts the closes of driver descriptors closeOpFD has
	// handed to goroutines of their own and whose onClose is not queued yet
	// (celeris#735). Shutdown waits for it (waitDriverCloses).
	driverClosers sync.WaitGroup

	// shutdownDriverHold keeps every driverConn handed to shutdownDrivers
	// reachable until the Worker itself is collected, which is after the ring
	// has been closed. The kernel may still own a RECV pointing into dc.buf at
	// that moment: shutdownDrivers deliberately does not wait for inflightOps
	// to settle (the loop is ending and would never process those CQEs), so
	// without this the buffers become garbage while an op is still live —
	// the celeris#256 class, where GC repurposed memory the kernel then wrote
	// HTTP bytes into. Closing the ring is what actually cancels the ops;
	// this only has to outlive that.
	shutdownDriverHold []*driverConn

	// reapRetry holds the (fd, generation) identities whose hand-off reap
	// missed, or found the SQ ring full, while their recv was still armed;
	// retryReaps re-runs them once per loop iteration (celeris#657).
	// reapRetrySpare is the second buffer of the swap. Worker thread only.
	reapRetry      []uint64
	reapRetrySpare []uint64
	// asyncCancelFlags: probeAsyncCancelFlags found this kernel accepting
	// IORING_ASYNC_CANCEL_* flags (5.19+); false when it rejected them, when
	// it gave an answer the probe does not recognise, or when the probe got
	// no answer. A reap is only placed when it is true;
	// createWorkers copies the engine's answer. Read-only after init.
	asyncCancelFlags bool
	// dupFD duplicates the descriptor a hand-off moves: unix.Dup when nil.
	// A test seam, so a test can make the dup fail the way a process out
	// of descriptors does (EMFILE).
	dupFD func(fd int) (int, error)

	// transplant (#383 reverse) is non-nil while a drain-to-epoll is in progress.
	// Set by Engine.StartTransplant (controller goroutine), read on this worker's
	// own thread after each handleRecv; when set, an idle H1 conn at a clean
	// boundary is detached and handed to the target epoll engine.
	transplant atomic.Pointer[transplantTargetHolder]

	// listenAddr is the address listenFD was bound to, recorded on this
	// worker's thread before it signals ready (celeris#639). Past ready the
	// worker may already have closed listenFD — a context cancelled during
	// startup runs shutdown at once — so Listen publishes this instead of
	// asking the kernel about a descriptor number that may now belong to
	// something else. Written once before ready; read only after it.
	listenAddr net.Addr
}

// recvArmStats are the recv-arming witnesses behind celeris#484 / #560,
// exported through engine.EngineMetrics so an oracle can read them after
// Shutdown (celeris#586). They are direct atomic adds, not per-iteration
// batches like reqBatch: a batch flushed at the top of the loop is lost
// when the loop returns before its next pass, and these are per-event
// invariants whose decision rule is "one event refutes". They fire at
// most a handful of times per run, so the cache-line cost is irrelevant.
//
//   - resumeWhileCancelPending: drainDetachQueue took the resume branch
//     with the pause's ASYNC_CANCEL still marked pending
//     (recvCancelPending non-zero). Slightly over-approximates the window:
//     a cancel that missed stays counted until its own completion is
//     processed (handleRecvCancel), a loop pass or two (celeris#596).
//   - resumeWhileRecvInFlight: the subset of those where the cancelled
//     recv was still armed at the resume. This is the exact window #484
//     lived in — the only state in which a second arm can land on top of
//     a kernel-held recv — and the witness that the load reached it.
//   - armDeclined: prepareRecv was asked to arm while a recv was already
//     armed and declined (any caller; the #560 guard).
//   - doubleArmed: connState.recvOutstanding reached 2 — a second recv SQE
//     was PLACED for one connection, by prepareRecv or flushSendLink's
//     linked recv. Cannot fire through prepareRecv while the guard holds;
//     it is the control-build witness.
//   - cqeUnaccounted: a terminal udRecv CQE reached the live conn (same
//     generation) while recvOutstanding was already 0 — the kernel held a
//     recv the bookkeeping did not know about. This is the only witness
//     independent of recvArmed: a stale-false recvArmed lets the guard pass
//     a second arm with recvOutstanding going 0→1, never 2, and only the
//     second terminal CQE exposes it.
//
// The last four are the celeris#607 witnesses — the inbound direction of
// the same arming machinery, where the failure is a recv that is never
// re-armed rather than one armed twice:
//
//   - sqFullRecv: prepareRecv could not get an SQE. That is the only way
//     cs.needsRecv is ever set, so it bounds every stall below.
//   - stallEpisodes: a dirty connection that wanted a recv arm was passed
//     over by the dirty-list retry because a SEND was still outstanding.
//     Counted once per episode, on the transition into it, not once per
//     event-loop pass: the loop revisits a dirty conn thousands of times a
//     second and a per-pass counter would be both meaningless and hot.
//   - stallNanos / stallMaxNanos: the cumulative and worst-case wall time
//     those episodes lasted, measured from the first skipped pass to the
//     arm (or to the connection leaving the dirty list). The maximum is
//     the discriminating number: a stall that outlives the peer's read
//     budget is a connection that went silent, not one that went slow.
//   - linkedRecvArms / linkedRecvBlockedNanos / linkedRecvBlockedMax: the
//     OTHER way inbound can be made hostage to outbound, and the one that
//     actually fires. flushSendLink chains the next RECV behind the SEND
//     with IOSQE_IO_LINK, so the kernel does not start the recv until the
//     send completes. Measured at the send's completion, while the chained
//     recv is provably still queued (cs.recvLinked is cleared only by that
//     recv's own CQE), so the interval is the coupling itself and not the
//     peer's idleness. The maximum is again the number that matters: a
//     request/response cycle pays microseconds, a peer that stopped
//     reading makes it seconds. Arms are batched per event-loop iteration
//     and waits are only recorded past linkBlockedFloorNanos, because
//     flushSendLink and the send completion are both the per-request hot
//     path and neither may grow an atomic or a clock read.
type recvArmStats struct {
	resumeWhileCancelPending atomic.Uint64
	resumeWhileRecvInFlight  atomic.Uint64
	armDeclined              atomic.Uint64
	doubleArmed              atomic.Uint64
	cqeUnaccounted           atomic.Uint64
	sqFullRecv               atomic.Uint64
	stallEpisodes            atomic.Uint64
	stallNanos               atomic.Uint64
	stallMaxNanos            atomic.Uint64
	linkedRecvArms           atomic.Uint64
	linkedRecvBlockedNanos   atomic.Uint64
	linkedRecvBlockedMax     atomic.Uint64
}

// zcStats are the SEND_ZC exposure witnesses behind celeris#585 (the
// fabric A/B) and celeris#587 (the race tier on the ZC send state),
// exported through engine.EngineMetrics (celeris#591). They answer the
// one question those two cannot ask today: did the zero-copy branch run
// at all? A clean A/B result or a clean race run with submits == 0 is
// not evidence about SEND_ZC, it is evidence the code path was never
// entered.
//
//   - submits: an IORING_OP_SEND_ZC SQE was armed by prepSendSQE. The
//     add sits INSIDE the ZC arm, so the plain-SEND path that every
//     sub-sendZCMinBytes response takes gains nothing.
//   - notifs: a CQE_F_NOTIF completion was processed — the kernel
//     released the pinned send buffer. submits - notifs is the number
//     of buffers still pinned in DMA.
//   - inlineBytes: payload bytes written by the detached inline-egress
//     fast path's raw unix.Write(2), which bypasses the ring entirely
//     and therefore can never be zero-copy.
//   - ringBytes: payload bytes completed through ring sends. Fed from
//     the worker-local ringBytesBatch with one atomic per event-loop
//     iteration (see bytesWrittenBatch): the completion site is the
//     per-request hot path and must not grow an atomic.
//
// inlineBytes + ringBytes is the egress-fabric split of BytesWritten —
// without it a SEND_ZC throughput delta cannot be attributed, because a
// WebSocket workload can ship most of its bytes off-ring.
type zcStats struct {
	submits     atomic.Uint64
	notifs      atomic.Uint64
	inlineBytes atomic.Uint64
	ringBytes   atomic.Uint64
}

func (s *zcStats) noteSubmit() {
	if s != nil {
		s.submits.Add(1)
	}
}

func (s *zcStats) noteNotif() {
	if s != nil {
		s.notifs.Add(1)
	}
}

func (s *zcStats) noteInlineBytes(n uint64) {
	if s != nil {
		s.inlineBytes.Add(n)
	}
}

// noteRingBytes publishes a worker's accumulated ring-send bytes. Called
// once per event-loop iteration from the batch flush, never per send.
func (s *zcStats) noteRingBytes(n uint64) {
	if s != nil {
		s.ringBytes.Add(n)
	}
}

func (s *recvArmStats) noteResumeWhileCancelPending() {
	if s != nil {
		s.resumeWhileCancelPending.Add(1)
	}
}

func (s *recvArmStats) noteResumeWhileRecvInFlight() {
	if s != nil {
		s.resumeWhileRecvInFlight.Add(1)
	}
}

func (s *recvArmStats) noteArmDeclined() {
	if s != nil {
		s.armDeclined.Add(1)
	}
}

func (s *recvArmStats) noteDoubleArmed() {
	if s != nil {
		s.doubleArmed.Add(1)
	}
}

func (s *recvArmStats) noteCQEUnaccounted() {
	if s != nil {
		s.cqeUnaccounted.Add(1)
	}
}

// linkBlockedFloorNanos is the shortest linked-recv wait the celeris#607
// witness records. Below it the chain resolved inside an event-loop pass,
// which is the whole point of the chain; above it the connection was
// genuinely unable to read for that long.
const linkBlockedFloorNanos = int64(time.Millisecond)

func (s *recvArmStats) noteSQFullRecv() {
	if s != nil {
		s.sqFullRecv.Add(1)
	}
}

// noteStallEnd folds one completed recv-arming stall into the totals and
// the running maximum. The CAS loop is the only unbounded work in the
// witness, and it runs once per episode, off the per-request path.
func (s *recvArmStats) noteStallEnd(d int64) {
	if s == nil || d <= 0 {
		return
	}
	n := uint64(d)
	s.stallNanos.Add(n)
	for {
		cur := s.stallMaxNanos.Load()
		if n <= cur || s.stallMaxNanos.CompareAndSwap(cur, n) {
			return
		}
	}
}

// noteLinkedRecvBlocked folds one linked-recv wait into the totals and the
// running maximum, exactly as noteStallEnd does for the dirty-list stall.
func (s *recvArmStats) noteLinkedRecvBlocked(d int64) {
	if s == nil || d <= 0 {
		return
	}
	n := uint64(d)
	s.linkedRecvBlockedNanos.Add(n)
	for {
		cur := s.linkedRecvBlockedMax.Load()
		if n <= cur || s.linkedRecvBlockedMax.CompareAndSwap(cur, n) {
			return
		}
	}
}

// beginRecvStall opens a recv-arming stall episode on cs if one is not
// already open. Called from the dirty-list retry when it declines to act
// on a connection whose recv arm is owed, so the clock read happens once
// per episode and not once per pass (celeris#607).
func (w *Worker) beginRecvStall(cs *connState) {
	if cs.recvStallSince != 0 {
		return
	}
	cs.recvStallSince = time.Now().UnixNano()
	if w.recvArm != nil {
		w.recvArm.stallEpisodes.Add(1)
	}
}

// endRecvStall closes an open stall episode on cs. Called wherever the
// stall can be resolved: the arm lands, the conn is paused, or the conn
// leaves the dirty list (including teardown, via removeDirty). No-op when
// nothing is open, which is the overwhelmingly common case — one branch on
// a worker-local int64.
func (w *Worker) endRecvStall(cs *connState) {
	if cs.recvStallSince == 0 {
		return
	}
	d := time.Now().UnixNano() - cs.recvStallSince
	cs.recvStallSince = 0
	w.recvArm.noteStallEnd(d)
	recvStallProbe(cs, d)
}

// noteRecvPlaced records that a recv SQE was placed for cs at one of the
// two placement sites (prepareRecv, flushSendLink). Worker-thread-only.
func (w *Worker) noteRecvPlaced(cs *connState) {
	cs.recvOutstanding++
	if cs.recvOutstanding >= 2 {
		w.recvArm.noteDoubleArmed()
	}
	// celeris#715, validation builds only (recvtheft.Enabled is a false
	// constant otherwise): both placement sites call this right after the
	// recv's GetSQE, so the SQE placed last is the recv.
	if recvtheft.Enabled {
		cs.recvArmSeq.Set(w.ring.sqPlaced() - 1)
	}
}

// recvUnsubmitted reports whether cs's armed recv SQE is still in the SQ
// ring, not yet consumed by the kernel: it was placed after this worker's
// last submit. Validation builds only: recvArmSeq is recorded only there.
//
// Why the close paths ask (celeris#715 hypothesis (a), the celeris#685
// class). Such a recv names the descriptor NUMBER (fixed files are off), and
// the kernel resolves the number when the next submit issues the recv.
// finishClose and finishCloseDetached queued the recv's ASYNC_CANCEL behind it
// and closed the descriptor at once. If another thread's accept was given the
// freed number before this worker's next submit, and its connection's request
// was already in the socket (TCP_DEFER_ACCEPT hands over only connections that
// have sent), the recv read that request and completed under the closed
// conn's (fd, generation): staleConnCQE dropped it as stale_recv_data_closed,
// and the new connection's own recv then waited on an empty socket. The close
// paths now keep the number while the recv is owed (fdOwed); under
// -tags=validation they still
//   - count the close (recvtheft.CloseWithUnsubmittedRecv, the witness of
//     the precondition),
//   - park the worker when the close path returns, when a recvtheft trial is
//     armed (recvtheft.HoldAfterClose), and
//   - in the trial's control arm, submit the ring before the close
//     (recvtheft.SubmitBeforeClose), so the recv is issued while the number
//     still names this conn's socket.
//
// None of it exists in production: recvtheft.Enabled is a false constant
// there and cs.recvArmSeq is zero-size.
func (w *Worker) recvUnsubmitted(cs *connState) bool {
	return cs.recvArmed && w.ring != nil && int32(cs.recvArmSeq.Get()-w.ring.sqConsumed()) >= 0
}

// recvLinkedOwed reports whether cs's armed recv is the one chained behind a
// SEND (flushSendLink) and has not completed: the kernel consumed its SQE with
// the SEND's, but issues it, and resolves its descriptor number, only after
// the SEND completes, as task work on a DEFER_TASKRUN ring, which can still
// be queued when the SEND's CQE is read (celeris#685). recvLinked is cleared
// only by that recv's own completion, so the pair is exact.
func recvLinkedOwed(cs *connState) bool {
	return cs.recvArmed && cs.recvLinked
}

// recvTheftWitness counts a close path's celeris#715 / celeris#685
// preconditions (validation builds only; the caller is guarded by
// recvtheft.Enabled) and reports whether the close hold applies, and in
// which form: an unsubmitted recv (linked false) or a linked one still owed
// (linked true). The caller defers recvtheft.HoldAfterClose itself, so the
// hold runs when the close path returns.
func (w *Worker) recvTheftWitness(cs *connState) (hold, linked bool) {
	switch {
	case w.recvUnsubmitted(cs):
		recvtheft.NoteCloseWithUnsubmittedRecv()
		return true, false
	case recvLinkedOwed(cs):
		recvtheft.NoteCloseWithLinkedRecv()
		return true, true
	}
	return false, false
}

// retireRecvCancel accounts for one of cs's outstanding backpressure-pause
// ASYNC_CANCELs having resolved, whether by cancelling a recv (the recv's
// -ECANCELED) or by cancelling nothing (the cancel's own completion). The
// clamp is defensive: one cancel carries IORING_ASYNC_CANCEL_ALL and would
// produce two -ECANCELEDs if a connection ever held two recvs, which is the
// celeris#484 defect RecvDoubleArmed exists to assert against.
// Worker-thread only.
func retireRecvCancel(cs *connState) {
	if cs.recvCancelPending > 0 {
		cs.recvCancelPending--
	}
}

// noteRecvTerminal records the terminal udRecv CQE for a live cs. A CQE
// arriving with nothing outstanding is a recv the kernel held that the
// bookkeeping never counted. Worker-thread-only.
func (w *Worker) noteRecvTerminal(cs *connState) {
	if cs.recvOutstanding > 0 {
		cs.recvOutstanding--
		return
	}
	w.recvArm.noteCQEUnaccounted()
}

func newWorker(id, cpuID int, tier TierStrategy, handler stream.Handler,
	resolved resource.ResolvedResources,
	cfg resource.Config, reqCount *atomic.Uint64, activeConns *atomic.Int64, errs *errclass.Counters,
	asyncPromoted *atomic.Uint64, acceptPaused *atomic.Bool,
	acceptCount, closeCount, bytesRead, bytesWritten *atomic.Uint64) (*Worker, error) { //nolint:unparam // error return used by callers for future fallible init

	// Listen socket creation is deferred to run() (after CPU pinning and NUMA
	// binding) so that the kernel allocates socket internal buffers on the
	// worker's NUMA node. This eliminates cross-socket access for accept
	// queue operations on multi-socket systems.

	return &Worker{
		id:            id,
		cpuID:         cpuID,
		listenFD:      -1,
		wakeFD:        wakefd.New(-1),
		tier:          tier,
		sqpoll:        tier.SQPollIdle() > 0,
		sendZC:        tier.SupportsSendZC(),
		async:         cfg.AsyncHandlers,
		h1Only:        cfg.Protocol == engine.HTTP1 && !cfg.EnableH2Upgrade,
		conns:         make([]*connState, fixedFileTableSize),
		liveConns:     make([]int, 0, 1024),
		handler:       handler,
		resolved:      resolved,
		cfg:           cfg,
		logger:        cfg.Logger,
		reqCount:      reqCount,
		activeConns:   activeConns,
		errs:          errs,
		asyncPromoted: asyncPromoted,
		acceptCount:   acceptCount,
		closeCount:    closeCount,
		bytesRead:     bytesRead,
		bytesWritten:  bytesWritten,
		acceptPaused:  acceptPaused,
		wake:          make(chan struct{}),
		ready:         make(chan error, 1),
		h2cfg: conn.H2Config{
			MaxConcurrentStreams: cfg.MaxConcurrentStreams,
			InitialWindowSize:    cfg.InitialWindowSize,
			MaxFrameSize:         cfg.MaxFrameSize,
			MaxRequestBodySize:   cfg.MaxRequestBodySize,
			WriteTimeout:         cfg.WriteTimeout,
		},
		sockOpts: sockopts.Options{
			TCPNoDelay:  true,
			TCPQuickAck: true,
			SOBusyPoll:  50 * time.Microsecond,
			RecvBuf:     resolved.SocketRecv,
			SendBuf:     resolved.SocketSend,
		},
	}, nil
}

func (w *Worker) run(ctx context.Context) {
	// celeris#905: the worker changes state that belongs to its OS thread,
	// the CPU affinity and the NUMA memory policy below and the ring's task
	// context, so it never unlocks the thread. A goroutine that exits locked
	// takes its thread with it: the runtime terminates the thread instead of
	// handing it, still pinned to one CPU, to whatever goroutine runs next.
	// The main thread cannot exit; the runtime parks it for good instead, and
	// a worker often runs on it, so the affinity and the policy are also put
	// back on the way out.
	runtime.LockOSThread()
	w.runCtx = ctx

	// The save fails on a kernel with more than 1024 possible CPUs, more
	// than unix.CPUSet holds, where the pin still succeeds: a main thread the
	// worker ran on is then parked still pinned.
	if prev, err := platform.SaveThreadAffinity(); err == nil {
		defer func() { _ = prev.Restore() }()
	}
	// celeris#909: a worker planned unpinned, or whose pin failed, has cpuID
	// -1: no NUMA node to bind to, and NewRingCPU sets no SQPOLL affinity.
	w.pinOwnThread()

	// Bind memory allocations to this CPU's NUMA node before creating
	// the listen socket, ring, and buffers. This ensures the socket's
	// accept queue, mmap'd SQ/CQ rings, SQE arrays, and provided buffer
	// regions are all NUMA-local to the worker thread, eliminating
	// cross-socket QPI/UPI traffic on multi-socket systems.
	if w.cpuID >= 0 {
		numaNode := platform.CPUForNode(w.cpuID)
		if err := platform.BindNumaNode(numaNode); err == nil {
			defer func() { _ = platform.ResetNumaPolicy() }()
		}
	}

	// Create the listen socket on the worker's NUMA node. Each worker has its
	// own listen socket via SO_REUSEPORT; kernel allocates socket internals
	// (accept queue, buffers) on the current thread's NUMA node.
	listenFD, err := createListenSocket(w.cfg.Addr, !w.cfg.DisableDeferAccept)
	if err != nil {
		w.ready <- fmt.Errorf("worker %d: listen socket: %w", w.id, err)
		return
	}
	w.listenFD = listenFD
	w.deferCapable = !w.cfg.DisableDeferAccept

	// celeris#656: from here until ready, a failure must close what this
	// worker has created. shutdown is the only other code that closes a
	// worker's descriptors, and it runs from the event loop, which a worker
	// that never became ready does not reach; Listen joins the failed worker
	// and returns its error without ever publishing it. A listen socket left
	// open stays LISTENing in the port's SO_REUSEPORT group for the life of
	// the process, and the kernel keeps hashing a share of new connections
	// into a backlog that nobody accepts. The failure sites below release
	// before they report, so the socket is gone by the time Listen sees the
	// error; this defer covers any return that does not.
	initDone := false
	defer func() {
		if !initDone {
			w.releaseFailedInit()
		}
	}()

	// Create ring after LockOSThread — SINGLE_ISSUER requires all ring
	// operations from the same OS thread. NewRingCPU pins the kernel's SQPOLL
	// thread to the same CPU as this worker, ensuring NUMA-local SQ ring polling.
	ring, err := newWorkerRing(uint32(w.resolved.SQERingSize), w.tier.SetupFlags(), w.tier.SQPollIdle(), w.cpuID)
	if err != nil {
		w.releaseFailedInit()
		w.ready <- fmt.Errorf("worker %d ring setup: %w", w.id, err)
		return
	}
	w.ring = ring

	// Fixed files are OFF unless explicitly opted into, and that gate is
	// deliberate — see celeris#541.
	//
	// Until now they were off by ACCIDENT: prepMultishotAcceptDirect set
	// SOCK_CLOEXEC alongside IORING_FILE_INDEX_ALLOC, io_accept_prep rejects
	// that combination with -EINVAL, and the runtime probe read the rejection
	// as "this kernel refuses ACCEPT_DIRECT". So cs.fixedFile has never been
	// true on any kernel, and every branch gated on it is unexecuted code. An
	// audit of those branches found eleven defects, none refuted, including
	// the DEFAULT receive path: prepRecv has no fixed-file variant and never
	// sets the flags byte, so every connection would arm a recv against a raw
	// fd equal to its slot index, colliding with real sockets in the process.
	//
	// Relying on that accident is not safe. It depends on a kernel continuing
	// to reject a malformed SQE; one that tolerated it would silently switch
	// the whole broken path on. The SQE bug is fixed in this change, so the
	// interlock is now this gate rather than an -EINVAL, and completing the
	// feature means working through celeris#541's checklist and removing this
	// block — not discovering the flags bug and assuming that was all.
	if fixedFilesEnabled(w.tier.SupportsFixedFiles()) {
		if err := w.ring.RegisterFiles(fixedFileTableSize); err != nil {
			w.logger.Warn("fixed file table registration failed, falling back",
				"worker", w.id, "err", err)
		} else {
			w.fixedFiles = true
			w.logger.Warn("fixed files enabled via "+envFixedFiles+": this path is INCOMPLETE "+
				"(celeris#541) — the default single-shot recv omits IOSQE_FIXED_FILE and will "+
				"read from unrelated descriptors. Do not use outside development.",
				"worker", w.id)
		}
	}

	// Multishot recv + ring-mapped provided buffers is OFF by default.
	//
	// Under sustained HTTP/1 Connection:close churn against a client
	// that pools connections (Go's http.Client with keep-alive of
	// course does NOT pool closed conns, but load balancers, service
	// mesh sidecars, and raw-socket benchmarking tools like goceleris/
	// loadgen all hold a pool of open conns and expect the server to
	// close them), multishot recv on aarch64 kernel 6.6.10 throttles
	// the whole worker to ~30 accepts/s / ~90 req/s — a 100× collapse
	// vs the epoll engine's ~25 k req/s on the identical workload.
	// The profile showed workers drowning in spurious recv CQEs (25 k
	// per worker per second against ~50 useful completions), and
	// disabling multishot recv in favour of single-shot per-conn
	// recv (the same model epoll uses) recovered churn to ~35 k rps
	// while costing ≈2 % on keep-alive simple and being a wash on
	// json-64k / body / headers.
	//
	// Opt back in with CELERIS_IOURING_MULTISHOT_RECV=1 for workloads
	// that are known to be dominated by long-lived keep-alive conns
	// and that benefit from the CQE-batching multishot provides.
	//
	// The ring size scales with the worker's per-conn target (celeris#322):
	// the previous hard-coded 1024 entries was undersized above ~1024 conns
	// and produced CQE storms as the kernel stalled waiting for buffer
	// returns. The formula gives 2 buffers per conn at the per-worker conn
	// target — comfortable headroom without runaway RSS.
	// CELERIS_IOURING_PBUF_COUNT overrides the auto-scaled value.
	if w.tier.SupportsMultishotRecv() && os.Getenv("CELERIS_IOURING_MULTISHOT_RECV") == "1" {
		bufRingCount := resolveBufRingCount(w.resolved, defaultConnsPerWorker)
		br, err := NewBufferRing(w.ring, bufRingGroupID, bufRingCount, w.resolved.BufferSize)
		if err != nil {
			w.logger.Warn("ring-mapped buffer registration failed, using per-connection buffers",
				"worker", w.id, "err", err, "buf_ring_count", bufRingCount)
		} else {
			w.bufRing = br
		}
	}

	// Create eventfd for H2 write queue wakeup. Handler goroutines signal
	// the eventfd after enqueuing response frames; io_uring POLL_ADD on the
	// eventfd wakes the ring event-driven, replacing the 100μs polling timeout.
	efd, efdErr := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if efdErr != nil {
		efd = -1
	}
	// celeris#655: producers hold the handle, never the number, so the
	// descriptor cannot be written once shutdown has closed it.
	if !w.wakeFD.Set(efd) && efd >= 0 {
		_ = unix.Close(efd)
	}

	w.prepareAccept()
	if _, err := submitInitialAccept(w.ring); err != nil {
		// celeris#656: the ring, the eventfd and any buffer ring exist now
		// too, and nothing past this return would close them either.
		w.releaseFailedInit()
		w.ready <- fmt.Errorf("worker %d initial submit: %w", w.id, err)
		return
	}

	// celeris#639: listenFD is certainly this worker's socket here; after
	// ready a cancelled context closes it in shutdown.
	w.listenAddr = listenAddrOf(w.listenFD)
	// From ready on, shutdown owns these descriptors (celeris#656).
	initDone = true
	w.ready <- nil
	w.cachedNow = time.Now().UnixNano()

	for {
		if ctx.Err() != nil {
			// celeris#595: a response produced by a handler that was
			// still running when the context was cancelled has only been
			// PREPARED at this point — flushSend sets cs.sending when it
			// writes the SEND SQE, but the submit that hands it to the
			// kernel happens further down this same loop, AFTER this
			// check. Tearing down here closed the fd with the response
			// still sitting in the SQ ring, so a request in flight at
			// Server.Shutdown died as a connection reset instead of
			// completing. Keep pumping the loop — which submits those
			// SQEs and reaps their completions through the normal path —
			// until no send is queued or in flight, bounded (below) so a
			// peer that stopped reading cannot hold shutdown open. The
			// loop keeps accepting for the first shutdownSendDrainNanos of
			// that window — it is the ordinary iteration — which is the
			// graceful side of the trade: a connection that arrives
			// inside it is answered rather than reset.
			//
			// First, though, the HTTP/2 stream handlers on the shared
			// worker pool (celeris#759): they run off this loop, and their
			// responses come back through the conns' write queues, which
			// only this loop drains. The send drain's clock starts once they
			// are done. That wait can last the whole budget, so it accepts
			// nothing (stopAccepting): a connection accepted in it was
			// served and then cut at the budget. net/http's Shutdown closes
			// its listeners first.
			//
			// The drain lasts as long as the budget of the last
			// Engine.Shutdown (drainEnd, celeris#806), and never less than
			// shutdownSendDrainNanos; every wait in this loop is bounded
			// meanwhile (draining), so a send the peer does not take cannot
			// keep the worker in the kernel past that end. Past the floor
			// the loop stops accepting, as it does while it waits for the
			// HTTP/2 handlers: a connection accepted in a drain that can
			// last the whole budget would be served and then cut at its end.
			// It reads no more request from an HTTP/1 connection either
			// (refuseRecv): on keep-alive connections whose responses queue
			// in the worker, steady traffic would keep the queues from ever
			// emptying and the drain would last the whole budget (or, for a
			// ctx without one, for as long as the clients asked).
			w.draining = true
			if !w.h2PoolSettled() {
				w.stopAccepting(ctx)
				w.shutdownDrainStart = 0
			} else {
				now := time.Now().UnixNano()
				if w.shutdownDrainStart == 0 {
					w.shutdownDrainStart = now
				}
				if !w.hasPendingSends() {
					w.shutdown()
					return
				}
				if end, bounded := w.drainEnd(w.shutdownDrainStart, shutdownSendDrainNanos); bounded && now > end {
					w.noteSendDrainGaveUp(now - w.shutdownDrainStart)
					w.shutdown()
					return
				}
				if now > w.shutdownDrainStart+shutdownSendDrainNanos {
					w.stopAccepting(ctx)
					w.refuseRecv = true
					w.stopDetachedProducers()
				}
			}
		}

		// ACTIVE → LINGERING → DRAINING (celeris#662): a pause first clears
		// TCP_DEFER_ACCEPT on this worker's listener and keeps accepting
		// until its linger deadline, then cancels the accept SQE and closes
		// the listener. See stepAcceptPause.
		// Cache the atomic load: same value used by the two branches
		// below and (further down) the SUSPENDED check. Saves 2 atomic
		// loads per event-loop iteration on the steady-state hot path.
		paused := w.acceptPaused.Load()
		if w.listenFD >= 0 && (paused || w.lingerUntil != 0) {
			// Cold: a pause is starting, lingering, ending, or being
			// withdrawn by a resume.
			w.stepAcceptPause(ctx, paused)
		}
		// Maintain the listenFDClosed signal that PauseAccept waits on:
		// true exactly while paused with no listen fd. That covers the
		// close stepAcceptPause just did and an fd that was already -1
		// from a prior Pause-Resume cycle that hadn't re-created the
		// socket yet (the worker may be mid-iteration when paused flips
		// back to true). It is false while the worker lingers, because the
		// listener is still open and accepting, and false once paused goes
		// false so a subsequent Pause observes a fresh signal.
		w.listenFDClosed.Store(paused && w.listenFD < 0)

		// SUSPENDED → ACTIVE: re-create listen socket after ResumeAccept;
		// never once shutdown has begun (stopAccepting).
		if w.listenFD < 0 && !paused && ctx.Err() == nil {
			fd, err := createListenSocket(w.cfg.Addr, !w.cfg.DisableDeferAccept)
			w.deferCapable = !w.cfg.DisableDeferAccept
			if err != nil {
				// This worker is about to stop accepting for good, so
				// the bump is not a rate: it is the one record that
				// the engine lost a listener (celeris#645).
				w.errs.ListenerRecreate.Add(1)
				w.logger.Error("re-create listen socket", "worker", w.id, "err", err)
				w.shutdown()
				return
			}
			w.listenFD = fd
			w.prepareAccept()
			if _, err := w.ring.Submit(); err != nil {
				w.logger.Error("submit after listen re-create", "worker", w.id, "err", err)
			}
		}

		// A previous accept re-arm that hit a full SQ ring is retried here,
		// before this iteration's submit, so it rides the same syscall.
		w.rearmAcceptIfPending()
		w.rearmH2PollIfPending()
		// celeris#657 P10 (A2w): keep the wake eventfd's POLL_ADD armed in
		// EVERY mode, not only for H2/h2c/driver/Detach work. Without it
		// wakeFD.Signal() cannot end a plain HTTP/1 worker's ring wait, and
		// a standby worker with no listen socket waits the full second
		// adaptiveTimeout gives it: measured, the first hand-off after an
		// idle revert came 1001.8-1008.8 ms late and the 500 ms idle test
		// failed 8 of 8 runs, against 0.7-5.7 ms and 0 of 8 with these
		// lines. One outstanding POLL_ADD per worker; handleH2Wakeup
		// re-arms it.
		if !w.h2PollArmed && w.wakeFD.FD() >= 0 {
			w.h2PollArmed = w.prepareH2Poll()
		}

		var cqHead, cqTail uint32
		if w.sqpoll {
			// SQPOLL path: kernel thread submits SQEs from the shared ring.
			// We never call Submit() — just clear the pending counter.
			w.ring.ClearPending()

			// Wake SQPOLL thread if it went idle after sqThreadIdle ms.
			if w.ring.SQNeedWakeup() {
				_ = w.ring.WakeupSQPoll()
			}

			// Check CQ ring — if CQEs ready, process without syscall.
			cqHead, cqTail = w.ring.BeginCQ()
			if cqHead == cqTail {
				// No CQEs — wait with adaptive timeout.
				if err := w.ring.WaitCQETimeout(w.adaptiveTimeout()); err != nil {
					w.shutdown()
					return
				}
				cqHead, cqTail = w.ring.BeginCQ()
			}
		} else {
			// Non-SQPOLL path: 4-mode adaptive submit.
			// 1. CQEs already ready + pending SQEs → Submit() (lightweight, no wait)
			// 2. CQEs already ready, nothing pending → skip syscall entirely
			// 3a. No CQEs + SENDs pending → SubmitAndWait() (no ext_arg)
			// 3b. No CQEs + no SENDs → SubmitAndWaitTimeout() (ext_arg + timeout)
			//
			// Modes 1 and 3a avoid ext_arg overhead: the kernel skips hrtimer
			// setup/teardown and sigset parsing, saving ~200-500ns per call.
			// Mode 3a is safe because SEND SQEs always produce CQEs (no
			// CQE_SKIP_SUCCESS), guaranteeing SubmitAndWait returns promptly.
			// Mode 3b uses a timeout because the only pending SQEs may have
			// CQE_SKIP_SUCCESS (e.g., CLOSE), and external events (recv, accept)
			// need a timeout for graceful shutdown via ctx.Err() checks.
			hasPending := w.ring.Pending() > 0
			cqHead, cqTail = w.ring.BeginCQ()
			if cqHead != cqTail && hasPending {
				// Mode 1: CQEs ready + SQEs pending. SubmitAndWait combines
				// submission + CQE retrieval into one syscall (saves ~300ns vs
				// separate Submit + next-iteration wait).
				if err := w.ring.SubmitAndWait(); err != nil {
					w.shutdown()
					return
				}
				cqHead, cqTail = w.ring.BeginCQ()
			} else if cqHead != cqTail { //nolint:revive // intentional no-op: CQEs ready, no pending SQEs, no syscall needed
			} else if waitUntimed(hasPending, w.sendsPending, w.draining) {
				// Mode 3a: SEND SQEs pending — guaranteed CQE on completion.
				// SubmitAndWait avoids ext_arg overhead (no hrtimer, no sigset).
				// Not once the context is cancelled (celeris#806): a SEND to a
				// peer that does not read has no completion to wait for, and
				// the drain's end is checked only between waits. The timed
				// wait below, capped by adaptiveTimeout, bounds it.
				if err := w.ring.SubmitAndWait(); err != nil {
					w.shutdown()
					return
				}
				cqHead, cqTail = w.ring.BeginCQ()
			} else if hasPending || cqHead == cqTail {
				// Mode 3b: no guaranteed CQEs — wait with adaptive timeout for
				// shutdown checks and CQE_SKIP_SUCCESS operations.
				if err := w.ring.SubmitAndWaitTimeout(w.adaptiveTimeout()); err != nil {
					w.shutdown()
					return
				}
				cqHead, cqTail = w.ring.BeginCQ()
			}
		}
		w.sendsPending = false

		if cqHead != cqTail {
			w.emptyIters = 0 // Reset adaptive timeout on activity.
			// Read the clock once per CQE batch, after the wait that
			// delivered it: every stamp this batch takes (an accept's, a
			// recv's, a send completion's lastActivity) is then no older
			// than the batch, as on epoll, which reads it on every
			// events-bearing epoll_wait return. It was read every 64th
			// iteration, and an idle or paused worker waits up to 100 ms
			// or 1 s per iteration, so its stamps could be seconds old,
			// older still after a park, and checkTimeouts, which compares
			// them with a fresh time.Now(), read that age as idle time:
			// past ReadTimeout it closed a connection in the middle of
			// steady traffic (celeris#713). One vDSO call per batch, not
			// per request.
			w.cachedNow = time.Now().UnixNano()
			now := w.cachedNow
			for cqHead != cqTail {
				entry := w.ring.cqeAt(cqHead)
				// Inlined CQE dispatch — eliminates processCQE method call
				// and avoids passing context.Context on every CQE (only
				// udAccept needs it). Decode op+fd once per CQE; the
				// previous form re-decoded fd from entry.UserData inside
				// each case. Hot ops (recv/send) are checked first.
				ud := entry.UserData
				fd := int(ud & fdMask)
				switch ud & udMask {
				case udRecv:
					// Generation gate (review 2.6): drop a stale CQE from a
					// prior fd occupant (recycling its provided buffer)
					// before routing to the handler.
					if !w.staleConnCQE(entry, fd, ud) {
						w.handleRecv(entry, fd, now)
						// #383 reverse: if a drain-to-epoll is in progress, this
						// conn just finished a request and may be at a clean
						// boundary — try to transplant it back to epoll.
						if w.transplant.Load() != nil {
							w.tryTransplant(fd)
						}
						// No releaseHold here (celeris#657): a held conn has
						// no recv armed, so the only recv completion that can
						// find a hold is the one whose own response set it,
						// with that SEND still in flight. The SEND completion
						// below is where a hold is released.
					}
				case udSend:
					if !w.staleConnCQE(entry, fd, ud) {
						w.handleSend(entry, fd, now)
						// celeris#587, validation builds only (zcwindow.Enabled
						// is a false constant otherwise, so this compiles away):
						// hold the SEND_ZC first-CQE -> NOTIF window open right
						// after handleSend has recorded the first completion and
						// released cs.detachMu, before anything else is released
						// (internal/zcwindow.SetHold).
						if zcwindow.Enabled && cqeHasMore(entry.Flags) {
							zcwindow.Hold()
						}
						// #383 reverse: io_uring flushes the response
						// asynchronously, so the clean, fully-flushed boundary
						// is reached HERE (send completed) — not at udRecv where
						// the send is still in flight. Try to transplant now.
						if w.transplant.Load() != nil {
							w.tryTransplant(fd)
						}
						// A held conn's SEND completion is where it either
						// left (above) or gets its recv back (celeris#657).
						w.releaseHold(fd)
					}
				case udAccept:
					w.handleAccept(ctx, entry, fd, now)
				case udClose:
					if !w.staleConnCQE(entry, fd, ud) {
						w.handleClose(fd)
					}
				case udH2Wakeup:
					w.handleH2Wakeup()
				case udHeaderTimer:
					// MUST be in the inlined hot path — processCQE
					// (which has the same case) is only called from
					// the cancel path. Without this case, slowloris-
					// defence timer CQEs are silently dropped.
					if !w.staleConnCQE(entry, fd, ud) {
						w.handleHeaderTimer(fd)
					}
				case udRecvCancel:
					// Same rule as udHeaderTimer: this case must exist in
					// the inlined dispatch or the recv-pause cancel's
					// failure CQE is dropped and recvCancelPending goes
					// stale again (celeris#596).
					if !w.staleConnCQE(entry, fd, ud) {
						w.handleRecvCancel(entry, fd)
					}
				case udTransplantReap:
					// And for the hand-off's reap: its miss is the only
					// event that says the recv is still armed (celeris#657).
					if !w.staleConnCQE(entry, fd, ud) {
						w.handleTransplantReap(entry, fd)
					}
				case udDriverRecv:
					w.handleDriverRecv(entry, fd)
				case udDriverSend:
					w.handleDriverSend(entry, fd)
				case udDriverClose:
					w.handleDriverClose(fd)
				}
				cqHead++
			}
		}
		w.ring.EndCQ(cqHead)

		// SENDs queued during CQE processing remain in the SQ ring and are
		// submitted at the top of the next iteration. For non-SQPOLL, the
		// adaptive submit combines them into a single submit+wait syscall.
		// For SQPOLL, the kernel thread picks them up from the shared ring
		// and ClearPending is called at the top of the SQPOLL path.

		// Flush batched request count to the shared atomic counter. This
		// replaces per-request atomic.Add with one atomic per CQE batch,
		// eliminating cache-line bouncing under multi-worker contention.
		if w.reqBatch > 0 {
			w.reqCount.Add(w.reqBatch)
			w.reqBatch = 0
		}

		// Flush batched payload-byte counters with the same per-iteration
		// cadence as reqCount, for the same cache-line-contention reason.
		if w.bytesReadBatch > 0 {
			w.bytesRead.Add(w.bytesReadBatch)
			w.bytesReadBatch = 0
		}
		if w.bytesWrittenBatch > 0 {
			w.bytesWritten.Add(w.bytesWrittenBatch)
			w.bytesWrittenBatch = 0
		}
		// Same cadence for the ring share of those bytes (celeris#591).
		if w.ringBytesBatch > 0 {
			w.zc.noteRingBytes(w.ringBytesBatch)
			w.ringBytesBatch = 0
		}
		// Same cadence for the celeris#685 deferred-close rate. (flushBatches
		// is this block for shutdown; a new batch goes in both, celeris#874.)
		if w.closeFDDeferredBatch > 0 {
			if w.handoffLoss != nil {
				w.handoffLoss.closeFDDeferred.Add(w.closeFDDeferredBatch)
			}
			w.closeFDDeferredBatch = 0
		}
		// Same cadence for the celeris#607 link-arm exposure witness.
		if w.linkArmBatch > 0 {
			if w.recvArm != nil {
				w.recvArm.linkedRecvArms.Add(w.linkArmBatch)
			}
			w.linkArmBatch = 0
		}

		// Single atomic publish for all batched buffer returns (P0).
		if w.hasBufReturns {
			w.bufRing.PublishBuffers()
			w.hasBufReturns = false
		}

		// Drain H2 async write queues FIRST. Handler goroutines enqueue
		// response frame bytes; draining them before the dirty list ensures
		// SEND SQEs are queued as early as possible after CQE processing,
		// reducing pipeline stalls for H2 multiplexed streams.
		// By index, from the end: a close below swap-removes the conn from
		// h2Conns, which a range would then skip one entry past.
		for i := len(w.h2Conns) - 1; i >= 0; i-- {
			fd := w.h2Conns[i]
			cs := w.conns[fd]
			if cs != nil && cs.h2State != nil && cs.h2State.WriteQueuePending() {
				cs.h2State.DrainWriteQueue(cs.writeFn)
				if cs.writeRefused {
					// A frame refused on back-pressure is lost, and the
					// connection's framing with it: close, sending what
					// was staged first (celeris#761).
					w.closeConn(fd)
					continue
				}
				if w.flushSend(cs) {
					w.markDirty(cs)
				}
			}
		}

		// Advance the monotonic iteration counter and release any
		// connStates whose deferred-release window has elapsed (see
		// queuePendingRelease docstring).
		w.iterCount++
		if len(w.pendingRelease) > 0 {
			// An idle iteration with a descriptor still kept for an owed op
			// (celeris#685) refreshes the clock the backstop reads: cachedNow
			// moves only with CQE traffic or the timeout sweep, and a kept
			// descriptor also keeps this worker from parking.
			if w.closeFDOwed > 0 && w.emptyIters > 0 {
				w.cachedNow = time.Now().UnixNano()
			}
			w.drainPendingRelease()
		}

		// Drain detached goroutine writes. Goroutines append to the queue
		// instead of calling markDirty directly (dirtyHead is worker-local).
		w.drainDetachQueue()

		// celeris#657 P9: re-examine what the drain still holds. After
		// drainDetachQueue, so a claim made by the previous pass's
		// Broadcast is already settled, and through the unchanged
		// tryTransplant, so the R0 gate, the reap and the hold all apply.
		w.sweep()

		// Apply driver-side actions (RegisterConn / UnregisterConn / Write)
		// on the worker thread so SQE submission honors single-issuer.
		if w.hasDriverConns.Load() || w.driverActionPending.Load() != 0 {
			w.drainDriverActions()
		}

		// Retry pending sends and dropped recv arms on dirty connections
		// (SQ ring was full earlier). Typically empty under normal load.
		w.flushDirty()

		// SENDs queued during CQE processing are submitted at the top of
		// the next iteration: Mode 1 SubmitAndWait combines submit + CQE
		// retrieval in one syscall. No separate submit needed here.

		// Increment empty iterations counter when no CQEs were found.
		// (Reset to 0 above when CQEs are present.)
		if w.emptyIters < 200 {
			w.emptyIters++
		}

		// Check connection timeouts. Default cadence is every 1024
		// iterations (~100ms under load); the gate tightens to every 32
		// iterations (~50ms idle wall time) in two cases:
		//   - detached conns exist with idle deadlines → WS idle-close
		//     must fire within its configured budget.
		//   - ReadHeaderTimeout > 0 → slowloris defence. The per-conn
		//     IORING_OP_TIMEOUT is the primary mechanism but it can
		//     silently drop arms under SQ-ring pressure (armHeaderTimer
		//     returns without arming if GetSQE+Submit retry still fails).
		//     The sweep is the belt-and-braces fallback; under heavy
		//     legit traffic (observability+static_swagger_proxy) the
		//     0x3FF gate is too coarse and the walker times out before
		//     the sweep notices. Tightening to 0x1F caps the worst-case
		//     close latency on a dropped arm to ~50ms.
		w.tickCounter++
		gate := uint32(0x3FF)
		if w.detachedCount > 0 || w.cfg.ReadHeaderTimeout > 0 {
			gate = 0x1F
		}
		if w.tickCounter&gate == 0 {
			w.checkTimeouts()
		}

		// DRAINING → SUSPENDED: no listen socket, no connections, CQEs processed.
		// Checked after CQE processing so accept CQEs for connections that
		// completed before the listen socket close are served, not leaked.
		//
		// hasDriverConns gate: connCount only counts HTTP conns. An
		// EventLoopProvider driver may still have live conns in
		// w.driverConns even when connCount==0; suspending the worker would
		// park its event loop and starve those driver conns of CQE
		// servicing. Stay active while any driver conn is registered (v1.5.0
		// review 2.10).
		// driverActionPending gate (celeris#624): AdoptConn hands a
		// transplanted descriptor to this worker by queuing a
		// driverActionAdopt and returns success immediately — the source
		// engine has ALREADY relinquished the conn by then. This park is
		// indefinite (a Go channel only ResumeAccept closes), so parking
		// on a queued adopt would lose that descriptor exactly the way the
		// epoll side lost one on its own detach queue: open, owned by
		// nobody, no hook, no close. The same flag covers a queued driver
		// register/unregister/write.
		//
		// Tested here alone, though, the flag is not enough: AdoptConn runs
		// on another goroutine, so it can queue between this test and the
		// park, and its eventfd write cannot end a park. That lost wakeup is
		// celeris#658 — the adopt sat queued on a standby worker until the
		// next ResumeAccept, forever if none came. So the flags are
		// re-checked under wakeMu below, and every driver-action enqueue
		// kicks a parked worker through wakeIfSuspended; see there for why
		// the pair cannot lose a wakeup.
		//
		// closeFDOwed gate (celeris#685): a close that left its descriptor
		// open until the kernel's last op on it completes is finished by
		// drainPendingRelease at that op's terminal CQE, which only an
		// iteration of this loop reads. Parking first would hold the
		// descriptor, and the socket with it, for as long as the park lasts.
		if w.listenFD < 0 && w.connCount == 0 && !w.hasDriverConns.Load() &&
			w.driverActionPending.Load() == 0 && w.detachQPending.Load() == 0 &&
			w.closeFDOwed == 0 && w.acceptPaused.Load() {
			// Submit what this iteration queued before parking (celeris#657,
			// A5). The iteration that closes or hands off the last conn
			// queues its close-path cancels (the header timer's, a send's)
			// in the same pass that finds the worker idle, and the park is
			// indefinite: those SQEs used to sit unsubmitted until something
			// woke the worker, measured 1-24 pending at parks. Outside
			// wakeMu, which is a leaf. Not under SQPOLL, where the kernel's
			// SQ thread submits; an SQ thread that has gone idle would need
			// the NEED_WAKEUP kick the submit branch of this loop gives it,
			// but no tier enables SQPOLL today (SQPollIdle is 0 in all
			// three), so that case is not handled here.
			if !w.sqpoll && w.ring.Pending() > 0 {
				_, _ = w.ring.Submit()
			}
			// A parked worker holds nothing, and sweep() — whose empty-set
			// retraction is what clears this worker's share of the residual
			// gauges while it runs — does not run again until it wakes.
			// sweep() runs before checkTimeouts here, so every Read, Idle or
			// Write timeout close of a draining worker's last connection
			// used to leave its residue standing in TransplantResidual* for
			// as long as the park lasted (celeris#711). Retract here, where
			// the sweep stops.
			if len(w.liveConns) == 0 {
				w.sweepRetract()
			}
			w.wakeMu.Lock()
			if !w.acceptPaused.Load() ||
				w.driverActionPending.Load() != 0 || w.detachQPending.Load() != 0 {
				w.wakeMu.Unlock()
				continue
			}
			w.suspended.Store(true)
			wake := w.wake
			w.wakeMu.Unlock()

			select {
			case <-wake:
			case <-ctx.Done():
				w.shutdown()
				return
			}
			continue
		}
	}
}

// staleConnCQE reports whether c is a late/in-flight CQE from a PRIOR
// occupant of fd (review 2.6). A CQE is stale when the slot is empty
// (cs == nil) or the conn currently at fd has a different generation than
// the one stamped into the CQE's user_data at SQE-submission time. Only
// conn-bound ops (udRecv/udSend/udClose/udHeaderTimer) carry a generation;
// callers must not invoke this for non-conn-bound ops.
//
// As the single chokepoint every conn-bound CQE passes through, this is
// also where kernelInflight accounting happens (v1.4.15/7beebb9 corruption fix): a TERMINAL
// recv/send CQE — one without CQE_F_MORE; multishot recv and SEND_ZC
// post intermediate F_MORE CQEs, and a cancel op's own CQE is never a
// udRecv/udSend op (the close-path cancels carry udProvide and are
// dropped before reaching here; the recv-pause cancel carries
// udRecvCancel and does pass through, for the generation gate alone) —
// decrements the owning conn's in-flight op count. For a live conn that
// is cs directly; for a stale CQE the closed conn is resolved through
// Worker.closedOps, keeping the
// bookkeeping on the OLD connState captured at arm time rather than
// whatever currently occupies w.conns[fd]. The decrement is what lets
// drainPendingRelease return the closed connState to the pool.
//
// CRITICAL: a stale recv CQE may still own a provided ring buffer. Dropping
// it without returning the buffer leaks a ring entry → ENOBUFS → the
// CQE-storm regression (celeris#322). When stale, we therefore recycle the
// buffer exactly as handleRecv's unknown/closing branch does (PushBuffer +
// hasBufReturns, published once per loop pass). The recycle is keyed on the
// CQE flags, not the conn, so it is correct even though the conn is gone.
func (w *Worker) staleConnCQE(c *completionEntry, fd int, ud uint64) bool {
	var cs *connState
	if fd >= 0 && fd < len(w.conns) {
		cs = w.conns[fd]
	}
	op := ud & udMask
	terminalOp := (op == udRecv || op == udSend) && !cqeHasMore(c.Flags)
	if cs != nil && cs.generation == decodeGen(ud) {
		if terminalOp {
			if op == udRecv {
				cs.recvArmed = false
				w.noteRecvTerminal(cs)
			}
			// KNOWN RESIDUAL — gen-collision misroute. A closed
			// predecessor's terminal CQE that arrives after this fd was
			// re-occupied by a conn with a COLLIDING generation (the
			// generation is the process-wide 32-bit connGenSeq, so this
			// needs 2^32 accepts while the predecessor's op is still
			// owed; see connGenSeq for the windows that allow it) is
			// indistinguishable from the live conn's own CQE and lands
			// here. The clamp below only stops an already-drained
			// counter going negative; a misdecrement from >=1 DOES
			// under-count the live conn (and may clear recvArmed above
			// with its recv still kernel-armed), so its own close can
			// skip the cancel and release early — the UAF class this
			// accounting exists to prevent. Reachability is narrow: a
			// close path keeps the number allocated until the last op
			// that names it has ended (celeris#685), so on this worker the
			// number cannot be re-occupied while a recv or send CQE of the
			// closed conn is still to come. Only a SEND_ZC notification
			// can be (celeris#798: it names no descriptor, and a stalled
			// peer holds it), which leaves this window where it was
			// before celeris#685 for that one CQE; any other needs the
			// pendingRelease backstop to have closed a number with an op
			// still owed (CloseFDForced, must stay 0). Either way the
			// gen collision comes ON TOP. When it fires, the closed conn's
			// closedOps entry is left orphaned and the 5 s backstop WARN
			// in drainPendingRelease is the production signal. If the CQE
			// taken was the closed conn's SEND_ZC notification, its zcOwed
			// stays set and the backstop holds that connState for good
			// instead (celeris#812, CloseZCNotifHeld): one connState
			// leaked, the safe side of the trade.
			if cs.kernelInflight > 0 {
				cs.kernelInflight--
			}
		}
		return false
	}
	// A stale recv that read bytes threw away bytes some client sent
	// (for a handed-off conn, a request that client is still waiting
	// on; see handoffLossStats for what each class means). Count it by
	// its identity's class BEFORE noteStaleTerminalOp can retire that
	// identity (celeris#657).
	if op == udRecv && c.Res > 0 {
		w.noteStaleRecvData(ud)
		// celeris#715, validation builds only: record what the stale recv
		// read, while closedOps still holds its connState.
		if recvtheft.Enabled {
			w.noteStaleRecvExemplar(c, fd, ud)
		}
	}
	if terminalOp {
		// A notification ends an op that named no descriptor any more
		// (celeris#798); every other terminal CQE ends one that did.
		w.noteStaleTerminalOp(ud, !cqeIsNotif(c.Flags))
	} else if op == udSend {
		// F_MORE on a send is a SEND_ZC's first CQE: the send is done, so it
		// names the descriptor no more, and its notification is still owed.
		w.noteStaleSendCompleted(ud)
	}
	if cqeHasBuffer(c.Flags) && w.bufRing != nil {
		w.bufRing.PushBuffer(cqeBufferID(c.Flags))
		w.hasBufReturns = true
	}
	return true
}

// noteStaleTerminalOp attributes a terminal recv/send CQE that arrived
// after its conn closed to the closed connState(s) registered under the
// CQE's (fd, generation) identity, releasing them for drainPendingRelease
// once the kernel owes them nothing. No-op when the identity is unknown
// (conn closed with zero in-flight ops, or already backstop-released).
// namedFD says whether the op still named the descriptor until this CQE
// (closedOpsEntry.fdOps): false only for a SEND_ZC notification.
//
// A send's terminal CQE also ends a SEND_ZC's hold on the send buffer
// (closedOpsEntry.zcOwed, celeris#812) when it is the SEND_ZC's: its
// notification (namedFD false), or, for a SEND_ZC whose first CQE came
// without IORING_CQE_F_MORE and so has no notification to follow, that first
// CQE. The latter looks like a plain send's CQE, so it is taken for the
// SEND_ZC's only where the identity holds one conn, whose one send in flight
// it is. Under an (fd, generation) collision only a notification ends a hold:
// holding late costs memory, releasing early sends a peer another
// connection's bytes. Whatever the CQE changed, the send buffers held for the
// identity past the backstop are then settled (settleZCHolds): the lookup
// that costs is paid only while some hold exists.
func (w *Worker) noteStaleTerminalOp(ud uint64, namedFD bool) {
	if len(w.closedOps) == 0 {
		return
	}
	key := connOpKey(ud)
	e := w.closedOps[key]
	if e == nil {
		return
	}
	e.inflight--
	if namedFD && e.fdOps > 0 {
		e.fdOps--
	}
	if ud&udMask == udSend && e.zcOwed > 0 && (!namedFD || len(e.conns) == 1) {
		e.zcOwed--
	}
	if len(w.zcHolds) > 0 {
		w.settleZCHolds(key, e)
	}
	if e.inflight > 0 {
		return
	}
	for _, cs := range e.conns {
		// nil: a conn whose SEND_ZC hold released it and kept its array
		// alone (releaseHeldConnState); the pool may have handed it on.
		if cs != nil {
			cs.kernelInflight = 0
		}
	}
	delete(w.closedOps, key)
}

// noteStaleSendCompleted takes a closed identity's SEND_ZC off its count of
// ops that name the descriptor at the send's first CQE (celeris#798): the op
// is done with the descriptor, and only its notification, which still
// counts in inflight, is owed. Worker thread only.
func (w *Worker) noteStaleSendCompleted(ud uint64) {
	if len(w.closedOps) == 0 {
		return
	}
	if e := w.closedOps[connOpKey(ud)]; e != nil && e.fdOps > 0 {
		e.fdOps--
	}
}

func (w *Worker) processCQE(ctx context.Context, c *completionEntry, now int64) {
	ud := c.UserData
	op := decodeOp(ud)
	fd := decodeFD(ud)

	switch op {
	case udRecv:
		if w.staleConnCQE(c, fd, ud) {
			return
		}
		w.handleRecv(c, fd, now)
		// The same hand-off attempt as the inlined dispatch (celeris#657).
		// The listener-close harvest processes completions here. As there,
		// no hold can be released at a recv completion; the udSend case
		// below releases it.
		if w.transplant.Load() != nil {
			w.tryTransplant(fd)
		}
	case udSend:
		if w.staleConnCQE(c, fd, ud) {
			return
		}
		w.handleSend(c, fd, now)
		// celeris#587: the same validation-only window hold as the inlined
		// dispatch; compiles away in production.
		if zcwindow.Enabled && cqeHasMore(c.Flags) {
			zcwindow.Hold()
		}
		if w.transplant.Load() != nil {
			w.tryTransplant(fd)
		}
		// A held conn whose SEND completion lands in the listener-close
		// harvest would otherwise be neither handed off nor re-armed.
		w.releaseHold(fd)
	case udClose:
		if w.staleConnCQE(c, fd, ud) {
			return
		}
		w.handleClose(fd)
	case udAccept:
		w.handleAccept(ctx, c, fd, now)
	case udH2Wakeup:
		w.handleH2Wakeup()
	case udHeaderTimer:
		if w.staleConnCQE(c, fd, ud) {
			return
		}
		w.handleHeaderTimer(fd)
	case udRecvCancel:
		if w.staleConnCQE(c, fd, ud) {
			return
		}
		w.handleRecvCancel(c, fd)
	case udTransplantReap:
		if w.staleConnCQE(c, fd, ud) {
			return
		}
		w.handleTransplantReap(c, fd)
	case udDriverRecv:
		w.handleDriverRecv(c, fd)
	case udDriverSend:
		w.handleDriverSend(c, fd)
	case udDriverClose:
		w.handleDriverClose(fd)
	}
}

// handleRecvCancel processes the CQE of the WebSocket backpressure pause's
// ASYNC_CANCEL (udRecvCancel) and retires cs.recvCancelPending when that
// cancel turns out to have cancelled nothing.
//
// With IORING_ASYNC_CANCEL_ALL the result is the NUMBER of ops cancelled:
//
//   - res > 0: the recv was cancelled and its -ECANCELED is on the way. This
//     cancel is NOT retired here — that -ECANCELED retires it, in handleRecv,
//     which reads the count to tell a pause still in force (return, stay
//     unarmed) from a pause the middleware already withdrew (re-arm, the
//     celeris#484 fix) apart from a genuine I/O error, which closes the conn.
//   - res == 0, or -ENOENT on a kernel that reports the miss as an error:
//     nothing matched. The recv had already completed on its own — with data,
//     before the cancel ran — or was never armed, and NO -ECANCELED will ever
//     arrive. This is the miss, and this CQE is the only event that can
//     observe it.
//   - -EALREADY: the op was found with cancellation already under way, so its
//     -ECANCELED is still coming. Treated as res > 0.
//
// Exactly one retirement per cancel either way, which is what lets several
// cancels be outstanding at once without their outcomes stealing each other's
// state (see connState.recvCancelPending).
//
// Until celeris#596 this cancel used the CQE_SKIP_SUCCESS form, which
// suppresses completions with res >= 0 — that is, precisely the res == 0 that
// reports the miss, so the miss produced no CQE at all. recvCancelPending then
// stayed set from the first missed cancel until some later cancel actually
// landed, and the resume branch, which reads it to witness the celeris#484
// window, counted every resume in between: 10 993 against 1 real entry at
// MaxBackpressureBuffer=8.
//
// Off the per-request path entirely: this op exists only while a detached
// WebSocket is in backpressure.
func (w *Worker) handleRecvCancel(c *completionEntry, fd int) {
	if fd < 0 || fd >= len(w.conns) {
		return
	}
	cs := w.conns[fd]
	if cs == nil {
		return
	}
	if c.Res > 0 || c.Res == -int32(unix.EALREADY) {
		return
	}
	retireRecvCancel(cs)
}

// handleHeaderTimer processes an IORING_OP_TIMEOUT CQE submitted by
// armHeaderTimer. The kernel fires it at the absolute deadline regardless
// of CQE traffic on the worker, giving slowloris defence parity with
// std's SetReadDeadline-based enforcement. The CQE is always processed —
// no res check — because:
//   - res == -ETIME on timer expiry (normal)
//   - res == -ECANCELED if cancelled (we don't cancel, only let it fire)
//   - res != 0 on any other error (we still need to clear headerTimerArmed)
//
// Race-free against ProcessH1's ClearHeaderDeadline: HeaderDeadlineNs is
// atomic; if cleared between SQE submission and CQE arrival, the close
// gate below skips the close. If re-armed in the same window (next
// keep-alive request), the ArmHeaderDeadline callback submits a fresh
// timer SQE — so we don't need to re-arm here.
// handleHeaderTimer ignores the caller's `now` because cachedNow is
// refreshed only every 64 iterations (60+ms stale under bursty load)
// and a stale now < dl comparison would spuriously re-arm a fresh 10s
// timer, effectively doubling the slowloris window. One vDSO call per
// timer CQE — rare in the hot path — is the right trade-off.
func (w *Worker) handleHeaderTimer(fd int) {
	if fd < 0 || fd > w.maxFD {
		return
	}
	cs := w.conns[fd]
	if cs == nil {
		return
	}
	cs.headerTimerArmed = false
	if cs.closing {
		return
	}
	// Snapshot under detachMu — same TOCTOU as checkTimeouts (celeris#548) and
	// the same TryLock for the same reason (celeris#593): this runs on the
	// LockOSThread'd worker, and runAsyncHandler holds detachMu for the whole
	// of ProcessH1. See snapshotH1Deadlines.
	snap, ok := snapshotH1Deadlines(cs)
	if !ok {
		// detachMu held ⇒ the conn is inside its handler, not waiting for a
		// request line, so the header deadline cannot be live. Dropping this
		// timer costs nothing: checkTimeouts re-reads HeaderDeadlineNs on its
		// ~50ms slowloris cadence and is the documented fallback for a timer
		// that failed to arm. Re-arming here instead would spin — the spec's
		// deadline has already passed, so the fresh timer would fire at once.
		return
	}
	if !snap.haveH1 {
		return
	}
	dl := snap.hdrDL
	if dl == 0 {
		// Headers completed before the timer fired; no-op. The next
		// ArmHeaderDeadline (keep-alive next request) will submit a
		// fresh timer SQE.
		return
	}
	now := time.Now().UnixNano()
	if now < dl {
		// True early fire (kernel clock drift, very rare). Re-arm
		// a fresh timer for the actual remaining time, from the
		// snapshot: the lock is released, so cs.h1State is not ours to
		// read again (celeris#722).
		w.armHeaderTimerAt(cs, dl)
		return
	}
	// Deadline exceeded — slowloris defence fires. Mirror std/net.http's
	// approach: plain unix.Close, no shutdown, no linger, no response.
	// net/http on hdrDeadline → isCommonNetReadError → "don't reply",
	// then c.close() → c.rwc.Close() (plain TCP close). Std walker shows
	// 0 hangs on the exact same probatorium test. We previously tried:
	//   - SHUT_RDWR + close + LINGER{1,0} (RST) → ~17-21 hangs per cell
	//   - 408 response + SHUT_WR + drain + close → ~17-25 hangs per cell
	// Both elaborate paths had the same ~5-7% walker-hang rate.
	// Suspect: SHUT_*/drain syscalls + LINGER each create their own
	// timing race vs. the walker's drip Write; plain close is simpler
	// and matches the observed-working std behavior exactly.
	w.closeConn(fd)
}

// armHeaderTimer submits an IORING_OP_TIMEOUT SQE that fires at
// cs.h1State.HeaderDeadlineNs. Called from initProtocol on conn-accept
// and from the OnHeaderDeadlineArmed callback when ProcessH1 re-arms
// for a keep-alive next request.
//
// Idempotent: if a timer is already in flight (cs.headerTimerArmed),
// the new arm is a no-op — the in-flight timer's CQE will check the
// current HeaderDeadlineNs and either close or re-submit as needed.
// Without this guard a fast-cycling keep-alive client could queue many
// in-flight timer SQEs for the same conn, exhausting the SQ ring.
func (w *Worker) armHeaderTimer(cs *connState) {
	if cs.h1State == nil || cs.headerTimerArmed {
		return
	}
	w.armHeaderTimerAt(cs, cs.h1State.HeaderDeadlineNs.Load())
}

// armHeaderTimerAt is armHeaderTimer for a deadline the caller has already
// read, and it does not read cs.h1State. On a promoted async conn cs.h1State
// belongs to the dispatch goroutine, which sets it to nil when the request is
// an h2c upgrade (switchToH2Local). So the worker reads the deadline where
// that goroutine cannot be running, and arms from the value it read
// (celeris#722): see asyncHeaderDeadline.
func (w *Worker) armHeaderTimerAt(cs *connState, dl int64) {
	if cs.headerTimerArmed || dl == 0 {
		return
	}
	now := time.Now().UnixNano()
	remaining := dl - now
	if remaining <= 0 {
		// Deadline already past — close immediately rather than queue
		// a zero-duration timer (kernel would fire it instantly anyway).
		w.closeConn(cs.fd)
		return
	}
	cs.headerTimerSpec.Sec = remaining / int64(time.Second)
	cs.headerTimerSpec.Nsec = remaining % int64(time.Second)
	sqe := w.ring.GetSQE()
	if sqe == nil {
		// Ring full — submit pending SQEs to drain, then retry.
		// Without this, high-CQE-traffic cells (observability,
		// static_swagger_proxy with concurrent Markov + adversarial
		// walkers) silently dropped a fraction of timer arms; those
		// conns then relied on the sweep, hitting the cadence floor.
		// Surfaced by nightly 26397557463 where slow refapps still
		// showed 62% slowloris hang rate post-forceRSTClose fix.
		if _, err := w.ring.Submit(); err != nil {
			return
		}
		sqe = w.ring.GetSQE()
		if sqe == nil {
			return
		}
	}
	prepTimeout(sqe, unsafe.Pointer(&cs.headerTimerSpec), 0, 0)
	setSQEUserData(sqe, encodeUserDataGen(udHeaderTimer, cs.fd, cs.generation))
	cs.headerTimerArmed = true
}

// adaptiveTimeout returns a wait timeout that scales with idle duration.
// Under load (CQEs arriving), returns 1ms for minimal latency. During idle
// periods, backs off to reduce syscall overhead (up to 100ms). When the
// listen socket is closed (draining), uses 1s. When H2 connections exist
// without eventfd, uses 100us for write queue polling. When detached
// conns exist with idle deadlines or ReadHeaderTimeout is configured, the
// cap drops to 25ms so checkTimeouts can fire detached-idle deadlines
// before expiry AND back up the per-conn IORING_OP_TIMEOUT for slowloris
// defence when the kernel timer SQE failed to arm under SQ-ring pressure.
// While an accept pause lingers, the wait is also capped at the time left to
// the linger's deadline, which an idle worker would otherwise overshoot by up
// to 100ms (celeris#662).
func (w *Worker) adaptiveTimeout() time.Duration {
	// Two caps apply on top of the base wait, and the result is the
	// smallest of the three: the celeris#657 sweep's next pass, and the
	// celeris#662 pause linger's deadline.
	d := w.sweptTimeout()
	// Waiting out HTTP/2 pool handlers at shutdown (celeris#759): a
	// handler that ends without a completion the ring hears of must not
	// leave the worker waiting.
	// And the send drain after it (celeris#806): the loop looks at the
	// drain's end between waits, and a wait ended by nothing at all is what
	// the peer that does not read gives it.
	if (w.draining || w.h2DrainStart != 0) && d > h2PoolDrainPoll {
		d = h2PoolDrainPoll
	}
	if w.lingerUntil != 0 {
		return capToDeadline(d, w.lingerUntil)
	}
	return d
}

// sweptTimeout is adaptiveTimeout with the celeris#657 sweep cap applied but
// not the celeris#662 pause linger's.
func (w *Worker) sweptTimeout() time.Duration {
	// A sweep pass is owed (celeris#657 P9): never wait past it. This is
	// the cap that matters, because the case the sweep exists for — a
	// standby worker with no listen socket and only idle keep-alives — is
	// exactly the one the 1 s below applies to.
	if d, owed := w.sweepWait(); owed {
		if w.listenFD < 0 || d < w.baseTimeout() {
			return d
		}
	}
	return w.baseTimeout()
}

// capToDeadline caps a ring-wait timeout at the time left to deadline (on
// deferlinger's monotonic clock), never below zero.
func capToDeadline(d time.Duration, deadline deferlinger.Deadline) time.Duration {
	return min(d, max(deadline.Left(), 0))
}

// baseTimeout is adaptiveTimeout without either cap.
func (w *Worker) baseTimeout() time.Duration {
	if w.listenFD < 0 {
		return 1 * time.Second
	}
	if len(w.h2Conns) > 0 && w.wakeFD.FD() < 0 {
		return 100 * time.Microsecond
	}
	if w.dirtyHead != nil {
		return 0
	}
	maxWait := 100 * time.Millisecond
	if w.detachedCount > 0 {
		maxWait = 50 * time.Millisecond
	}
	if w.cfg.ReadHeaderTimeout > 0 && maxWait > 25*time.Millisecond {
		maxWait = 25 * time.Millisecond
	}
	switch {
	case w.emptyIters <= 10:
		return 1 * time.Millisecond
	case w.emptyIters <= 100:
		d := 5 * time.Millisecond
		if d > maxWait {
			d = maxWait
		}
		return d
	default:
		return maxWait
	}
}

func (w *Worker) handleAccept(ctx context.Context, c *completionEntry, _ int, now int64) {
	if c.Res < 0 {
		// EINVAL with fixed files: ACCEPT_DIRECT not supported on this kernel.
		// Disable fixed files and retry with regular multishot accept.
		if c.Res == -22 && w.fixedFiles {
			w.logger.Warn("ACCEPT_DIRECT failed (EINVAL), disabling fixed files",
				"worker", w.id)
			w.fixedFiles = false
			if w.listenFD >= 0 {
				sqe := w.ring.GetSQE()
				if sqe != nil {
					prepMultishotAccept(sqe, w.listenFD)
					setSQEUserData(sqe, encodeUserData(udAccept, w.listenFD))
				}
			}
			return
		}
		// Classify by errno rather than folding every failed accept into
		// one number (celeris#645). The ECANCELED a PauseAccept leaves on
		// the in-flight multishot lands in ErrorAcceptCancelled, which is
		// what makes an adaptive switch's accept cost separable from a
		// sustained accept-side loss. The pause clears w.listenFD before it
		// handles that completion, so the re-arm below does not fire for
		// the descriptor being closed (celeris#662).
		w.errs.AcceptFailed(unix.Errno(-c.Res))
		// Re-arm accept whenever the kernel is not going to deliver more
		// CQEs from the current SQE. In single-shot mode !cqeHasMore is
		// always true (each accept produces exactly one CQE), so this
		// preserves the old single-shot re-arm. In multishot mode an error
		// CQE clears F_MORE to signal the multishot was terminated — and
		// without this re-arm the worker would permanently stop accepting
		// (the old `!SupportsMultishotAccept()` guard never re-armed in
		// multishot mode, silently killing accept on a transient ENOMEM /
		// EMFILE). See celeris v1.5.0 review 2.1.
		if w.listenFD >= 0 && !cqeHasMore(c.Flags) {
			w.prepareAccept()
		}
		return
	}

	newFD := int(c.Res)
	w.onAcceptedFD(ctx, newFD, now, w.fixedFiles)

	// Re-arm accept when CQE_F_MORE is clear. In single-shot mode this
	// fires on every CQE. In multishot mode kernel sets F_MORE=1 to say
	// "more CQEs coming from this SQE"; if F_MORE is clear the multishot
	// was terminated (kernel backpressure or error) and without re-arming
	// the worker permanently stops accepting on its listen socket.
	// Observed on aarch64 kernel 6.6.10: multishot accept silently
	// terminated under HTTP/1 Connection:close churn pressure, killing
	// accept throughput on that worker until the engine restarted.
	if !cqeHasMore(c.Flags) && w.listenFD >= 0 {
		w.prepareAccept()
	}
}

// onAcceptedFD sets up state for a newly accepted fd — builds connState,
// registers it with the worker, and arms the first recv.
func (w *Worker) onAcceptedFD(ctx context.Context, newFD int, now int64, isFixedFile bool) {
	// Bounds check: reject FDs outside the flat conn array.
	if newFD < 0 || newFD >= len(w.conns) {
		if !isFixedFile {
			_ = unix.Close(newFD)
		}
		w.errs.ConnTableCap.Add(1)
		return
	}

	if !isFixedFile {
		_ = sockopts.ApplyFD(newFD, w.sockOpts)
	}
	// For fixed files, socket options were applied by the kernel at accept time
	// via inherited options. TCP_NODELAY etc. must be set post-accept for
	// non-inherited options — but with fixed files (ACCEPT_DIRECT), the fd field
	// is actually a fixed file index and we can't call setsockopt on it directly.

	bufSize := w.resolved.BufferSize
	if w.bufRing != nil {
		bufSize = 0
	}
	connCtx := ctxkit.WithWorkerID(ctx, w.id)
	cs := acquireConnState(connCtx, newFD, bufSize, w.async)
	cs.fixedFile = isFixedFile

	if !isFixedFile {
		if sa, err := unix.Getpeername(newFD); err == nil {
			cs.remoteAddr = sockaddrString(sa)
		}
	}

	w.conns[newFD] = cs
	w.connCount++
	w.addLiveConn(cs)
	if newFD > w.maxFD {
		w.maxFD = newFD
	}
	cs.writeFn = w.makeWriteFn(cs)
	w.activeConns.Add(1)
	w.acceptCount.Add(1)

	if w.cfg.OnConnect != nil {
		w.cfg.OnConnect(cs.remoteAddr)
	}

	cs.lastActivity = now

	// H2C + EnableH2Upgrade is semantically "H2-first but accept H1→H2
	// upgrades too", so route it through the recv-time detection path (like
	// Auto) rather than locking cs.protocol=H2C on accept. Without
	// this, the first HTTP/1.1 upgrade request was fed to ProcessH2 and
	// the PRI-preface check silently failed, leaving the client with 27
	// bytes of server SETTINGS frame and no 101 Switching Protocols
	// response.
	if w.cfg.Protocol != engine.Auto &&
		(w.cfg.Protocol != engine.H2C || !w.cfg.EnableH2Upgrade) {
		cs.protocol.Store(int32(w.cfg.Protocol))
		cs.detected = true
		w.initProtocol(cs)
	} else if w.cfg.ReadHeaderTimeout > 0 {
		// The protocol is decided by the first bytes, so there is no H1 state
		// and no header deadline yet (initProtocol arms it at detection). The
		// conn gets the same absolute budget from accept: a client that sends
		// "GE" and stalls, or dribbles the HTTP/2 preface, is reaped by
		// checkTimeouts instead of living on the activity stamp each of its
		// bytes refreshes (celeris#974).
		cs.detectDeadline = now + int64(w.cfg.ReadHeaderTimeout)
	}
	if !w.prepareRecv(cs, cs.buf) {
		cs.needsRecv = true
		w.markDirty(cs)
	}
	// celeris#715, validation builds only: report the accept to an armed
	// recvtheft trial, which parks this worker here (its recv prepared, not
	// submitted) when it was given the number another worker's held close
	// released.
	if recvtheft.Enabled {
		recvtheft.AfterAccept(w.id, newFD)
	}
}

// stepAcceptPause runs one step of the accept pause on the worker thread,
// at the top of an iteration, while the listener is open and the engine is
// paused or this worker is lingering (celeris#662, celeris#675).
//
//   - ACTIVE → LINGERING (paused, no linger yet): clear TCP_DEFER_ACCEPT on
//     this worker's listener (and apply the tcp_synack_retries=0 guard when
//     the pause asked for it), then linger until deferlinger.Linger after
//     that clear. The multishot accept stays armed, so connections keep
//     being accepted through handleAccept: a connection deferred before the
//     clear is promoted by the kernel about a second after its SYN, and one
//     that arrives after the clear enters the accept queue at once. A
//     listener created without the option, or a zero linger, closes at
//     once instead: nothing on it can be deferred.
//   - LINGERING, before the deadline: nothing. adaptiveTimeout caps the
//     ring wait at the deadline.
//   - LINGERING → CLOSED at the deadline: closeListenerAfterDrain.
//   - LINGERING → ACTIVE (a resume arrived first): the option goes back on
//     the same descriptor, which is never closed and whose accept was never
//     cancelled.
//
// The worker sees the pause at its next ring wakeup, which on a plain
// HTTP/1 worker with nothing in flight can be its adaptive timeout (up to
// 100 ms): BeginPauseAccept arms no wakeup for it. Nothing depends on that
// latency, because the deadline is taken at this worker's own clear.
func (w *Worker) stepAcceptPause(ctx context.Context, paused bool) {
	switch {
	case !paused:
		deferlinger.Leave(w.listenFD, w.deferCapable, w.pause, w.logger, "worker", w.id)
		w.lingerUntil = 0
	case w.lingerUntil == 0:
		if !w.pause.Observed() {
			return // test hook only: model a worker that sees the pause late
		}
		if w.lingerUntil = deferlinger.Enter(w.listenFD, w.deferCapable, w.pause, w.logger, "worker", w.id); w.lingerUntil == 0 {
			w.closeListenerAfterDrain(ctx)
		}
	case w.lingerUntil.Passed():
		w.closeListenerAfterDrain(ctx)
	}
}

// closeListenerAfterDrain is the pause's close. It cancels the pending
// io_uring accept on the listen socket, handles the completions that
// produces, serves every connection still in the accept queue
// (acceptQueuedOnPause), and closes the socket. The cancel releases the
// kernel's io_uring reference to the underlying file, allowing the socket to
// leave the SO_REUSEPORT group immediately. Without it, unix.Close alone
// leaves a phantom socket that intercepts connections. It is called only
// while the engine is paused, so it sets listenFDClosed itself -- the value
// the iteration's own store sets right after -- before it wakes a
// PauseAccept waiting for it.
func (w *Worker) closeListenerAfterDrain(ctx context.Context) {
	// Clear w.listenFD BEFORE handling the cancel's completions.
	// handleAccept re-arms accept whenever listenFD >= 0 and a completion
	// arrives without F_MORE, which is exactly what the cancelled accept
	// delivers, so it used to queue a new accept SQE for the descriptor
	// this function then closed. That SQE went to the kernel after the
	// close and failed with EBADF on every pause, or with EINVAL when a
	// sibling worker's drain had already reused the number for one of its
	// connections (celeris#662): a stale accept aimed at a descriptor this
	// worker does not own.
	lfd := w.listenFD
	w.listenFD = -1
	w.cancelAccept(ctx, lfd)
	// The completions above only cover handshakes an accept had already
	// reached. Anything still in the kernel accept queue would be aborted
	// by the close below, after its client had possibly sent a request;
	// accept it now instead and serve it like any other connection
	// (celeris#662).
	w.acceptQueuedOnPause(ctx, lfd)
	_ = unix.Close(lfd)
	w.lingerUntil = 0
	deferlinger.NoteClose()
	w.listenFDClosed.Store(true)
	w.pause.Notify()
}

// cancelAccept cancels the accept armed on the listen socket lfd, which the
// caller has already taken out of w.listenFD, and handles the completions it
// produces before the caller closes lfd. The cancel releases the kernel's
// io_uring reference to the socket's file, so the close takes it out of the
// SO_REUSEPORT group at once; unix.Close alone left a phantom socket that
// intercepted connections.
func (w *Worker) cancelAccept(ctx context.Context, lfd int) {
	if sqe := w.ring.GetSQE(); sqe != nil {
		prepCancelFDSkipSuccess(sqe, lfd)
		setSQEUserData(sqe, 0)
		// Submit and wait for the cancel to complete before closing.
		_ = w.ring.SubmitAndWaitTimeout(50 * time.Millisecond)
		// Process CQEs: skip cancel completions (userData=0), handle
		// everything else normally to avoid breaking active connections.
		cancelNow := time.Now().UnixNano()
		cqH, cqT := w.ring.BeginCQ()
		for cqH != cqT {
			entry := w.ring.cqeAt(cqH)
			if entry.UserData != 0 {
				w.processCQE(ctx, entry, cancelNow)
			}
			cqH++
		}
		w.ring.EndCQ(cqH)
	}
}

// stopAccepting cancels the accept and closes the listener, once the
// worker's context is cancelled and it waits for the HTTP/2 stream handlers
// on the shared worker pool (celeris#759), or, past the first
// shutdownSendDrainNanos of its send drain (celeris#806). Either wait can
// last the whole budget, and the worker accepted and served new connections
// meanwhile, which were then cut at its end; net/http's Shutdown closes its
// listeners first. What is still in the kernel's accept queue is reset, as
// the close in shutdown did. Only then, not on every shutdown: the cancel
// submits the SQ ring and handles its completions, and a shutdown whose
// send drain is done within its first 250 ms goes straight on, leaving the
// driver conns' queued ops to shutdown (TestDriverShutdownReleasesDescriptors).
// Idempotent. Worker thread.
func (w *Worker) stopAccepting(ctx context.Context) {
	if w.listenFD < 0 {
		return
	}
	lfd := w.listenFD
	w.listenFD = -1 // before the cancel's completions: handleAccept re-arms on a listenFD >= 0
	w.cancelAccept(ctx, lfd)
	_ = unix.Close(lfd)
	w.lingerUntil = 0
}

// acceptQueuedOnPause accepts every connection still waiting in the listen
// socket's kernel accept queue before a pausing worker closes that socket
// (celeris#662). The pause cancels the multishot accept and handles the
// completions already posted, but a handshake no accept completion had reached
// is still queued in the kernel, and the close aborts it — after its client
// may have sent a request.
//
// Each descriptor goes to onAcceptedFD, the path an accept completion takes
// (as a plain descriptor: accept4 never returns a fixed-file index). So it is
// counted, OnConnect fires, sockopts apply, a descriptor past the conn table
// is closed and counted in ConnTableCap, and a first recv that a full SQ ring
// cannot take is left on the dirty list with needsRecv set, which the loop
// re-arms once the next submit has made room: nothing is dropped.
//
// It can only reach what the kernel ACCEPT QUEUE holds. While
// TCP_DEFER_ACCEPT is set the kernel keeps a handshake-complete connection
// that has sent no data out of that queue altogether, which is why a
// listener that has the option lingers before it gets here
// (stepAcceptPause): the option is cleared first, and by the deadline the
// kernel has promoted into this queue every connection it deferred before
// the clear.
//
// The listen socket is non-blocking (createListenSocket), so accept4 returns
// EAGAIN once the queue is empty; the drain is also bounded at the conn
// table's size. EMFILE/ENFILE or an unexpected error stops it and is
// classified by AcceptFailed, as an accept completion's would be; what is
// still queued is left to the close.
//
// Interactions: the drained connections hold connCount above zero, so the
// DRAINING→SUSPENDED gate keeps the worker serving them until they close or
// are transplanted, then parks it as before. A ResumeAccept racing the drain
// loses nothing: the worker still closes the listener it read as paused and
// re-creates it on the next iteration, as it did before.
//
// Residual states NOT covered, matching the list on epoll's
// acceptQueuedOnPause: a handshake still in progress at the close, and one
// that COMPLETES between the final EAGAIN and the close. Those are inherent
// to closing a listen socket; only the kernel's tcp_migrate_req can move
// them to another listener in the SO_REUSEPORT group. And a connection
// deferred before the clear whose promotion comes after the deadline, which
// takes a lost retransmitted SYN-ACK or a client that does not answer it.
//
// listenFD is the socket being closed. The caller has already cleared
// w.listenFD, so no completion handled on the way can re-arm accept on it.
func (w *Worker) acceptQueuedOnPause(ctx context.Context, listenFD int) {
	now := time.Now().UnixNano()
	for range len(w.conns) {
		newFD, _, err := unix.Accept4(listenFD, unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		if err != nil {
			switch err {
			case unix.EAGAIN:
				return
			case unix.EINTR, unix.ECONNABORTED:
				// A signal, or a queued connection reset before it was
				// taken: the rest of the queue is still there.
				continue
			}
			var errno unix.Errno
			_ = errors.As(err, &errno)
			w.errs.AcceptFailed(errno)
			return
		}
		w.onAcceptedFD(ctx, newFD, now, false)
	}
}

// errHijackNoSQE is what hijackConn answers when the SQ ring has no room for
// the cancel of the recv it would leave armed on the hijacker's socket.
var errHijackNoSQE = errors.New("celeris: cannot hijack: the io_uring submission queue is full, the connection's receive cannot be cancelled")

func (w *Worker) hijackConn(fd int) (net.Conn, error) {
	cs := w.conns[fd]
	if cs == nil {
		return nil, errors.New("celeris: connection not found")
	}
	if cs.fixedFile {
		return nil, errors.New("celeris: cannot hijack fixed file connection")
	}
	// Refuse on the async dispatch path (celeris#539). Everything below is
	// worker-owned: w.conns, w.connCount, w.liveConns, the dirty list, and an
	// ASYNC_CANCEL SQE, which the engine treats as single-issuer. In async
	// mode the handler — and therefore this call, through h1State.HijackFn —
	// runs on the per-connection dispatch goroutine, so doing any of it here
	// races the worker's own loop and submits an SQE off the issuing thread.
	//
	// Every other goroutine-to-worker hand-off goes through detachQueue for
	// exactly this reason. Hijack cannot use it as-is because Hijack() is
	// synchronous (internal/conn/response.go calls hijackFn and returns its
	// result to the handler), so it needs a blocking round trip with a
	// shutdown escape or the handler hangs when the worker stops. That is
	// tracked for v1.7.0; refusing is the safe behaviour until then, because
	// the alternative is silently corrupting worker state.
	if w.async {
		return nil, errors.New("celeris: Hijack is not supported on the io_uring engine with " +
			"AsyncHandlers enabled (celeris#539); use the engine-integrated WebSocket/SSE path, " +
			"or run with AsyncHandlers disabled")
	}
	if cs.sending || len(cs.sendBuf) > 0 || len(cs.writeBuf) > 0 {
		return nil, errors.New("celeris: cannot hijack with pending sends")
	}
	// Place the cancel of the recv still armed on the socket before anything
	// changes (celeris#869). The socket lives on under the hijacker, so nothing
	// but this cancel stops that recv from reading its first bytes (the close
	// paths shut the read side down; a hijack cannot), and the release backstop
	// must not be left to give the connState up with the recv still owed. With
	// no room in the SQ ring even after a submit the hijack is refused, the
	// conn untouched, and the handler can answer the request instead.
	// Only the recv's cancel is placed here, and the rest of the cancels
	// after the conn has left the tables: a refused hijack leaves the conn as
	// it was, its header timer included.
	if !w.cancelRecvOp(fd, cs) {
		return nil, errHijackNoSQE
	}
	// Unlink from the dirty list (celeris#527). Here it is not just a leak:
	// the fd stays open under the caller's net.Conn, and the dirty loop's
	// retry pass calls prepareRecv on whatever it walks — re-arming exactly
	// the recv this function cancels below to stop it stealing the
	// hijacker's first bytes.
	w.removeDirty(cs)
	w.removeLiveConn(cs)
	w.conns[fd] = nil
	w.connCount--
	w.activeConns.Add(-1)
	w.closeCount.Add(1)
	// Cancel-then-release discipline, hijack variant: any op still armed
	// on cs (a multishot recv stays armed across its request; the
	// single-shot recv that brought the request has completed, measured by
	// TestRecvTheft685HijackSingleShot) targets cs.buf. The fd lives on
	// under the caller's net.Conn, so an uncancelled recv would not only
	// pin cs.buf past release (the #256-class UAF) but also STEAL the
	// first bytes the hijacker tries to read. Cancel it by its
	// generation-tagged user_data and defer the pool release until the
	// terminal CQE arrives, exactly like finishClose.
	//
	// Then drop cs instead of recycling it (celeris#733): the request's
	// strings are views of cs.buf (or of cs.detectAccum, when the request
	// began in an earlier recv), and a hijacking handler typically keeps
	// the path, params and headers for the goroutine that serves the
	// connection. Recycled, cs would hand those buffers to the next
	// connection this worker accepts, which would receive its request into
	// them. The detached release holds cs until the kernel is done with
	// cs.buf and then leaves it to the garbage collector, which frees the
	// buffers once the last view of them is gone. The cost is one connState
	// allocation per hijack.
	//
	// On the multishot receive path (CELERIS_IOURING_MULTISHOT_RECV=1) the
	// request is not in cs.buf at all but in a provided-ring buffer, which
	// handleRecv retires after ProcessH1 returns ErrHijacked instead of
	// pushing it back (celeris#868): the same guarantee, one buffer
	// allocation per hijack.
	//
	// celeris#685 hijack witness and hold, validation builds only: count a
	// hijack with an op still owed on the socket (kernelInflight > 0), and
	// hold the worker thread before it returns, and so before its next
	// io_uring_enter (recvtheft.SetHijackHold).
	if recvtheft.Enabled {
		if cs.kernelInflight > 0 {
			recvtheft.NoteHijackWithOpOwed()
		}
		defer recvtheft.HijackHold()
	}
	// The recv's cancel was placed above, before the conn left the tables.
	// The result of the rest is not read: a pending send was refused above, so
	// what is left is a SEND_ZC notification (zcNotifPending), which
	// closedZCOwed holds the send buffer for, and the header timer, whose
	// cancel is best effort. Nothing marks a hijack entry cancelMissed.
	w.cancelOtherOps(fd, cs)
	w.noteClosedInflight(cs)
	w.queuePendingReleaseDetached(cs)
	// The fd-lifetime rule, hijack variant (celeris#685). The socket lives
	// on under the hijacker's net.Conn, so no op of this worker may read it
	// once the hijacker has it, and none may resolve the original number,
	// which the f.Close below releases. The cancel above reaches the kernel
	// only at the next submit. Until then an owed recv that is already
	// issued (a multishot recv stays armed across its request) can take the
	// hijacker's first bytes on a ring whose completions run at any syscall
	// exit, and one whose SQE is still in the ring would resolve the number
	// after the close. So when an op is owed, submit now: an issued recv is
	// cancelled before it can read, and a recv SQE still in the ring is
	// issued first, against this socket and not a reused number, then
	// cancelled. No hijack path leaves such an SQE: a hijack runs inside
	// its request's processing, after that request's recv completed. Nor a
	// linked recv, which rides behind a SEND: a hijack with a send pending
	// is refused above. One io_uring_enter per hijack with an op owed; none
	// otherwise, which is every hijack on single-shot recv.
	if fdOwed(cs) && !w.sqpoll {
		_, _ = w.ring.Submit()
	}
	f := os.NewFile(uintptr(fd), "tcp")
	c, err := net.FileConn(f)
	_ = f.Close()
	return c, err
}

func (w *Worker) initProtocol(cs *connState) {
	switch engine.Protocol(cs.protocol.Load()) {
	case engine.HTTP1:
		cs.h1State = conn.NewH1State()
		cs.h1State.RemoteAddr = cs.remoteAddr
		cs.h1State.MaxRequestBodySize = w.cfg.MaxRequestBodySize
		cs.h1State.OnExpectContinue = w.cfg.OnExpectContinue
		cs.h1State.EnableH2Upgrade = w.cfg.EnableH2Upgrade
		cs.h1State.WorkerID = int32(w.id)
		cs.h1State.WorkerIDSet = true
		// Slowloris defence: kernel-enforced per-conn header deadline.
		//
		// Sync mode (!w.async): ProcessH1 runs inline on the worker thread,
		// so OnHeaderDeadlineArmed → armHeaderTimer is always a worker-
		// thread call. Safe.
		//
		// Async mode (w.async): ProcessH1 runs on the per-conn dispatch
		// goroutine. The keep-alive re-arm path (ProcessH1 sees
		// HeaderDeadlineNs == 0 and calls ArmHeaderDeadline at line 360
		// of internal/conn/h1.go) would fire OnHeaderDeadlineArmed from
		// the goroutine, which would call w.ring.GetSQE — a violation of
		// IORING_SETUP_SINGLE_ISSUER that can silently drop SQEs or
		// corrupt the SQ ring. Leave OnHeaderDeadlineArmed nil in async
		// mode; the initial accept-time arm fires via the direct
		// armHeaderTimer call below (worker thread, safe), the recv-
		// path retry (handleRecv) covers SQ-pressure drops, and keep-
		// alive re-arms fall back to checkTimeouts which now runs every
		// 32 iters × 25ms = 800ms worst case.
		cs.h1State.ReadHeaderTimeoutNs = int64(w.cfg.ReadHeaderTimeout)
		if !w.async {
			cs.h1State.OnHeaderDeadlineArmed = func() { w.armHeaderTimer(cs) }
		}
		cs.h1State.ArmHeaderDeadline()
		if w.async {
			// Direct worker-thread arm for the initial deadline; the
			// nil OnHeaderDeadlineArmed above means ArmHeaderDeadline
			// only stamped HeaderDeadlineNs, not the SQE.
			w.armHeaderTimer(cs)
			// Per-handler async (celeris #300): wire the route resolver so
			// ProcessH1, when run inline on the worker (InlineMode), can
			// detect an async route and bail to the dispatch goroutine.
			// Only meaningful in async mode; gated on HasAsyncRoutes so
			// pure-sync servers running with Config.AsyncHandlers=true
			// don't pay the per-recv resolver call.
			if r, ok := w.handler.(stream.AsyncRouteResolver); ok && r.HasAsyncRoutes() {
				cs.h1State.RouteAsync = r.RouteAsync
			}
		}
		if !w.cfg.EnableH2Upgrade {
			cs.h1State.DisableH2CDetect()
		}
		// Back-pressure for HTTP/1 is held per request (celeris#761): a
		// request that finds the conn's unsent responses over the limit is
		// not served, and the conn is closed once they have gone out.
		cs.h1State.WriteBacklogged = cs.overBacklogH1
		// No zero-copy body writer (SetWriteBodyFn): the H1 response
		// adapter copies every body into writeBuf. A body the WRITEV path
		// left in place was read by the kernel only at the next
		// io_uring_enter, after the handlers of the other conns in the same
		// completion batch had run, and the body belongs to the handler,
		// which may reuse it as soon as its write returns: c.JSON puts its
		// buffer back in a pool at once, where the next handler to encode
		// takes it, so one conn's response went out with another's bytes
		// (celeris#817).
		cs.h1State.OnDetach = func() {
			// Async mode may have already allocated detachMu in
			// acquireConnState; reuse it so the async goroutine and the
			// middleware goroutine share one mutex. Otherwise create
			// a fresh mutex for the WS/SSE detach flow.
			mu := cs.detachMu
			if mu == nil {
				mu = &sync.Mutex{}
				cs.detachMu = mu
			}
			// Sync mode: OnDetach runs on the worker thread, so the
			// worker-owned bookkeeping is safe to mutate here.
			// Async mode: OnDetach runs on the per-conn dispatch
			// goroutine — w.detachedCount races with the worker
			// thread's adaptiveTimeout read and any ring SQE
			// submission violates SINGLE_ISSUER. Defer both to
			// drainDetachQueue via asyncDetachPending so the worker
			// owns the mutation. The first guarded() write below
			// enqueues+signals, so drainDetachQueue fires promptly.
			if !w.async {
				w.detachedCount++
				cs.detachCounted = true
				if w.detachedConns != nil {
					w.detachedConns.Add(1)
				}
			} else {
				cs.asyncDetachPending = true
			}
			orig := cs.writeFn
			wake := w.wakeFD
			guarded := func(data []byte) {
				mu.Lock()
				if cs.detachClosed {
					mu.Unlock()
					return
				}
				orig(data)
				// Inline egress fast path (io_uring, SINGLE_ISSUER-safe): the
				// ring may only be driven by the worker thread, but a raw
				// unix.Write(2) on the socket fd is legal from any goroutine iff
				// NO ring SEND is in-flight for this conn (else the two writes
				// interleave on the wire). detachMu (held) gates it: the worker
				// submits every SEND under detachMu and sets cs.sending, and
				// completeSend now clears it under detachMu too, so with
				// cs.sending==false && !zcNotifPending && sendBuf/bodyBuf empty no
				// SEND is outstanding and none can start while we hold the lock —
				// writeBuf is the only pending data, so the raw write is exclusive
				// and correctly ordered. Requires a REAL fd: under ACCEPT_DIRECT
				// (fixedFile) cs.fd is a ring file-table index, not a syscall'able
				// fd, so those conns skip the fast path and use the ring (same
				// guard hijack uses). This parallelises WS/SSE egress across the
				// dispatch goroutines like the std engine instead of funnelling
				// every send through the single worker thread. On full drain we
				// skip the worker handoff; on partial we compact the remainder to
				// the front (the worker's flushSend swaps writeBuf→sendBuf next);
				// on EAGAIN/error we leave writeBuf and fall through — the worker
				// ring-sends the rest and surfaces any I/O error via completeSend.
				if !cs.fixedFile && !cs.sending && !cs.zcNotifPending &&
					len(cs.sendBuf) == 0 && len(cs.bodyBuf) == 0 && len(cs.writeBuf) > 0 {
					if n, werr := unix.Write(cs.fd, cs.writeBuf); werr == nil {
						w.bytesWritten.Add(uint64(n))
						// celeris#591: these bytes never touch the ring, so
						// SEND_ZC cannot apply to them. InlineBytes vs
						// RingBytes is the egress-fabric split the #585 A/B
						// needs to attribute a throughput delta. One atomic
						// per inline write, alongside the bytesWritten add
						// that is already here — no per-request cost, this
						// path only exists for detached (WS/SSE) egress.
						w.zc.noteInlineBytes(uint64(n))
						if n >= len(cs.writeBuf) {
							cs.writeBuf = cs.writeBuf[:0]
							mu.Unlock()
							return
						}
						cs.writeBuf = cs.writeBuf[:copy(cs.writeBuf, cs.writeBuf[n:])]
					}
				} else if cs.zcNotifPending {
					// celeris#591: the fast path declined with a SEND_ZC
					// notification still outstanding — the kernel holds
					// cs.sendBuf pinned for DMA and an inline write here
					// would interleave with it on the wire. This is the
					// witness that the ZC-vs-inline window was entered at
					// all; a #587 race run that reports zero here never
					// reached the interleaving it set out to test. Under
					// detachMu (held), so not a racy read.
					validation.IouringInlineGuardBlockedZC.Add(1)
				}
				mu.Unlock()
				// Signal the event loop to flush the remainder. Do NOT call
				// markDirty from this goroutine — dirtyHead is worker-local.
				w.detachQMu.Lock()
				w.detachQueue = append(w.detachQueue, cs)
				// Edge-triggered wakeup: only the enqueue that takes the detach
				// queue empty->non-empty writes the wakeup eventfd. This Swap and
				// the drain's detachQPending.Store(0) both run under detachQMu, so
				// a racing enqueue is either captured by the drain's swap or
				// observes pending==0 and re-arms the wakeup — never both missed
				// (see drainDetachQueue). Coalesces the per-message wakeup-syscall
				// storm under a hot broadcast fan-out — previously one unix.Write
				// per message per connection. The detachMu-guarded writeBuf
				// mutation is untouched, so the WS-write-vs-flushSend ordering
				// invariant (celeris#284) is fully preserved.
				wasEmpty := w.detachQPending.Swap(1) == 0
				w.detachQMu.Unlock()
				if wasEmpty {
					wake.Signal()
				}
			}
			cs.writeFn = guarded
			// Also update the response adapter so StreamWriter writes
			// go through the guarded path (not the stale pre-Detach writeFn).
			cs.h1State.UpdateWriteFn(guarded)
			// Expose raw write for WebSocket (bypasses chunked encoding).
			cs.h1State.RawWriteFn = guarded
			// Install pause/resume callbacks for WebSocket backpressure.
			// They set a desired state and wake the worker; the actual
			// recv cancel / re-arm is performed in drainDetachQueue
			// (worker thread).
			cs.h1State.PauseRecv = func() {
				if cs.recvPauseDesired.Swap(true) {
					return
				}
				w.detachQMu.Lock()
				w.detachQueue = append(w.detachQueue, cs)
				// Coalesce the wakeup on the detach queue's empty->non-empty
				// edge — see the write closure above and drainDetachQueue.
				wasEmpty := w.detachQPending.Swap(1) == 0
				w.detachQMu.Unlock()
				if wasEmpty {
					wake.Signal()
				}
			}
			cs.h1State.ResumeRecv = func() {
				if !cs.recvPauseDesired.Swap(false) {
					return
				}
				w.detachQMu.Lock()
				w.detachQueue = append(w.detachQueue, cs)
				// Coalesce the wakeup on the detach queue's empty->non-empty
				// edge — see the write closure above and drainDetachQueue.
				wasEmpty := w.detachQPending.Swap(1) == 0
				w.detachQMu.Unlock()
				if wasEmpty {
					wake.Signal()
				}
			}
			// Ensure eventfd poll is armed so the worker wakes up.
			// Sync mode runs OnDetach on the worker thread, so this
			// SQE submission is safe. Async mode defers to
			// drainDetachQueue (worker-thread) via asyncDetachPending
			// — see the SINGLE_ISSUER note on the asyncDetachPending
			// field.
			if !w.async && !w.h2PollArmed && w.wakeFD.FD() >= 0 {
				w.h2PollArmed = w.prepareH2Poll()
			}
			// Async mode (HTTP1): the dispatch goroutine took detachMu
			// around ProcessH1 so writeBuf access serialises with the
			// worker's flushSend. Now that we've installed `guarded`
			// (which re-acquires detachMu on every call), keeping the
			// lock held would deadlock the very next write — including
			// the middleware-emitted 101 / SSE headers that immediately
			// follow Detach. Release the lock here and have the
			// dispatch goroutine observe asyncDetachUnlocked to skip
			// its symmetric Unlock when ProcessH1 returns. This is
			// celeris#273 — pre-fix, /ws and /events would TIMEOUT on
			// iouring + AsyncHandlers because the dispatch goroutine
			// was deadlocked on its own re-entrant Lock attempt.
			//
			// Gate on cs.asyncPromoted: detachMu is Locked around
			// ProcessH1 ONLY by runAsyncHandler (the dispatch goroutine),
			// which runs exclusively for PROMOTED conns. With per-handler
			// async (#300/#302) a not-yet-promoted async-mode conn runs
			// its FIRST request INLINE on the worker thread (tryInline);
			// a SYNC route — the WebSocket /ws upgrade or SSE /events
			// stream, which are not themselves .Async() — does not bail
			// with ErrAsyncDispatch, so its handler runs inline and calls
			// Context.Detach() → OnDetach while detachMu was NEVER Locked.
			// Unlocking here under the old w.async-only guard then faulted
			// with "sync: unlock of unlocked mutex", fatally crashing the
			// process on the first /ws or /events request. The
			// asyncPromoted gate restricts the Unlock to the dispatch-
			// goroutine path that actually holds the lock. See celeris#309.
			// Do NOT release detachMu here — defer it to AFTER Detached.Store(
			// true) below (mirrors the epoll fix e100873). A concurrent
			// closeConn (peer RST mid-upgrade) acquires detachMu before CloseH1
			// and re-reads Detached under it; holding the lock across the Store
			// guarantees closeConn observes the ownership handoff and skips
			// CloseH1 instead of recycling the Context/stream this WS/SSE
			// middleware goroutine is still using — the use-after-recycle that
			// nil-derefs WSRawWriteFn under a peer RST mid-upgrade.
			unlockDetachMu := w.async && cs.asyncPromoted.Load() && cs.detachMu != nil && !cs.asyncDetachUnlocked
			if unlockDetachMu {
				// Under asyncInMu: dispatchBusy reads it there, and from here
				// on this goroutine never holds detachMu across a handler
				// again, so the worker goes back to waiting out the (bounded)
				// holders of this conn's lock (celeris#704). detachMu is held
				// here; detachMu -> asyncInMu is the only order the two are
				// ever nested in.
				cs.asyncInMu.Lock()
				cs.asyncDetachUnlocked = true
				cs.asyncInMu.Unlock()
			}
			// Async mode: enqueue cs so drainDetachQueue picks up the
			// deferred bookkeeping (asyncDetachPending). The first
			// guarded() write will also enqueue+signal, but the
			// explicit signal here covers the rare case where the
			// middleware returns without writing (e.g. a 4xx Detach
			// rejection path). Idempotent — drainDetachQueue
			// clears asyncDetachPending after running it.
			if w.async {
				w.detachQMu.Lock()
				w.detachQueue = append(w.detachQueue, cs)
				// Coalesce the wakeup on the detach queue's empty->non-empty
				// edge — see the write closure above and drainDetachQueue.
				wasEmpty := w.detachQPending.Swap(1) == 0
				w.detachQMu.Unlock()
				if wasEmpty {
					wake.Signal()
				}
			}
			// Publish barrier: Store(true) LAST so a worker that observes
			// Detached.Load()==true is guaranteed (atomic happens-before) to
			// also see every detach side effect set above — detachMu install,
			// guarded writeFn/RawWriteFn, pause/resume callbacks, async
			// bookkeeping. In async mode OnDetach runs on the dispatch
			// goroutine while the worker reads Detached on the hot path
			// (writeCap, drainRecv, WS delivery); publishing it first would
			// let the worker act on a half-installed detach. See the data-race
			// fix making Detached atomic.
			cs.h1State.Detached.Store(true)
			// Release detachMu now that Detached is published, so the guarded
			// writeFn the middleware calls next (the 101 / SSE headers) can
			// re-acquire it. A closeConn blocked on detachMu now proceeds and
			// reads Detached==true, so it skips CloseH1.
			if unlockDetachMu {
				cs.detachMu.Unlock()
			}
		}
		if !cs.fixedFile {
			cs.h1State.HijackFn = func() (net.Conn, error) {
				return w.hijackConn(cs.fd)
			}
		}
	case engine.H2C:
		cs.h2State = conn.NewH2State(w.handler, w.h2cfg, cs.writeFn, w.wakeFD)
		cs.h2State.SetRemoteAddr(cs.remoteAddr)
		// Arm eventfd POLL_ADD on first H2 connection so the ring wakes
		// event-driven when handler goroutines enqueue responses.
		if !w.h2PollArmed && w.wakeFD.FD() >= 0 {
			w.h2PollArmed = w.prepareH2Poll()
		}
		w.h2Conns = append(w.h2Conns, cs.fd)
	}
}

// switchToH2 promotes an H1 connection to H2 mid-stream (RFC 7540 §3.2).
// Called after ProcessH1 returns ErrUpgradeH2C. Drops H1 state, builds H2
// state with the upgrade info pre-applied, and drains any residual bytes
// (which may contain the H2 client preface + initial SETTINGS) through
// ProcessH2 synchronously.
func (w *Worker) switchToH2(cs *connState) error {
	if err := w.switchToH2Local(cs); err != nil {
		return err
	}
	if !w.h2PollArmed && w.wakeFD.FD() >= 0 {
		w.h2PollArmed = w.prepareH2Poll()
	}
	w.h2Conns = append(w.h2Conns, cs.fd)
	return nil
}

// switchToH2Local does every part of switchToH2 except the worker-owned
// steps (w.h2Conns append, prepareH2Poll) which must run on the worker
// goroutine — that slice and SQE submission are SINGLE_ISSUER. The
// async dispatch goroutine uses this under cs.detachMu and then asks
// the worker to finish via asyncH2Promoted + detachQueue.
func (w *Worker) switchToH2Local(cs *connState) error {
	info := cs.h1State.UpgradeInfo
	h2State, err := conn.NewH2StateFromUpgrade(w.handler, w.h2cfg, cs.writeFn, w.wakeFD, info)
	if err != nil {
		cs.h1State.UpgradeInfo = nil
		conn.ReleaseUpgradeInfo(info)
		return err
	}
	cs.h1State.UpgradeInfo = nil
	conn.CloseH1(cs.h1State)
	cs.h1State = nil
	cs.h2State = h2State
	cs.h2State.SetRemoteAddr(cs.remoteAddr)
	cs.protocol.Store(int32(engine.H2C))

	var processErr error
	if len(info.Remaining) > 0 {
		processErr = conn.ProcessH2(cs.ctx, info.Remaining, cs.h2State, w.handler, cs.writeFn, w.h2cfg)
	}
	conn.ReleaseUpgradeInfo(info)
	return processErr
}

// closeOnRecvEnd is handleRecv's peer-FIN and recv-error tail: tell a
// detached middleware (OnError), then close. OnError runs under detachMu:
// cs.h1State is read under it because the async dispatch goroutine's
// switchToH2Local nils cs.h1State under the same lock, so reading it before
// acquiring detachMu is a TOCTOU that the race detector flags (#256
// regression class).
//
// When detachMu is held across a handler (dispatchBusy), waiting for the lock
// parked the worker, and every connection of its ring, until the handler
// returned (celeris#704); the common shape is a client that gives up on a
// slow handler and disconnects. So the notification rides on the close
// instead: closeConn leaves the close to the dispatch goroutine, which exits
// at its next check, and then runs it on this thread with the lock free,
// delivering closeErr first, or Worker.shutdown does when it gets there
// before the handler does (celeris#867).
func (w *Worker) closeOnRecvEnd(fd int, cs *connState, err error) {
	if mu := cs.detachMu; mu != nil {
		if !mu.TryLock() {
			if dispatchBusy(cs, nil) {
				// The first error is the one the middleware is told: a
				// second recv-end CQE for the same conn (the recv is not
				// retired by the first close request) must not replace it
				// (celeris#867).
				if cs.closeErr == nil {
					cs.closeErr = err
				}
				w.closeConn(fd)
				return
			}
			mu.Lock()
		}
		if cs.h1State != nil && cs.h1State.OnError != nil {
			cs.h1State.OnError(err)
		}
		mu.Unlock()
	}
	w.closeConn(fd)
}

func (w *Worker) handleRecv(c *completionEntry, fd int, now int64) {
	cs := w.conns[fd]
	if recvStallProbeActive && cs != nil {
		cs.lastRecvCQE = now
	}
	if cs == nil || cs.closing {
		// If multishot recv with provided buffers, batch-return the buffer
		// even for unknown/closing connections to prevent buffer leak (P0).
		if cqeHasBuffer(c.Flags) && w.bufRing != nil {
			w.bufRing.PushBuffer(cqeBufferID(c.Flags))
			w.hasBufReturns = true
		}
		return
	}
	// A hand-off reap is outstanding (celeris#657): its -ECANCELED is the
	// hand-off point and must never reach the generic negative-result
	// branch below, which would close a healthy conn.
	if cs.transplantReap > 0 && w.reapOutcome(c, fd, cs) {
		return
	}

	if c.Res <= 0 {
		if cqeHasBuffer(c.Flags) && w.bufRing != nil {
			w.bufRing.PushBuffer(cqeBufferID(c.Flags))
			w.hasBufReturns = true
		}
		// A zero-length recv completion is the peer's FIN, not a failure.
		// Surfacing it through errIORingRecv gave middleware unix.Errno(0),
		// which no error classifier recognises, so every ordinary disconnect
		// read as a protocol error (celeris#564).
		if c.Res == 0 {
			w.closeOnRecvEnd(fd, cs, errPeerClosed)
			return
		}
		// Recv was cancelled by drainDetachQueue (WS backpressure pause).
		// Don't close — the connection stays open until ResumeRecv re-arms.
		if c.Res == -int32(unix.ECANCELED) && cs.recvPaused {
			retireRecvCancel(cs)
			return
		}
		// Same cancel, but the middleware drained its buffer and withdrew
		// the pause before the cancel landed. The connection is healthy and
		// now unarmed, so re-arm it. Closing here — which is what the
		// generic negative-result path below does — tore down a working
		// connection whenever a burst was consumed faster than the pause
		// round trip (celeris#484).
		if c.Res == -int32(unix.ECANCELED) && cs.recvCancelPending > 0 {
			retireRecvCancel(cs)
			if !w.prepareRecv(cs, cs.buf) {
				cs.needsRecv = true
				w.markDirty(cs)
			}
			return
		}
		// ENOBUFS (-105): provided buffer ring exhausted. The multishot recv
		// is terminated by the kernel but the connection is healthy. Re-arm
		// multishot recv — buffers will be available after current batch is
		// returned via PublishBuffers().
		if c.Res == -105 && w.bufRing != nil {
			w.bufRing.PublishBuffers()
			if !cs.recvPaused {
				if !w.prepareRecv(cs, cs.buf) {
					cs.needsRecv = true
					w.markDirty(cs)
				}
			}
			return
		}
		// Surface read failure to detached middleware before closing.
		w.closeOnRecvEnd(fd, cs, errIORingRecv(c.Res))
		return
	}

	cs.lastActivity = now
	// Data: the conn is serving again, so a hand-off that failed at its dup
	// may be tried (and its recv reaped) once more (celeris#657).
	cs.reapSuppressed = false
	// c.Res > 0 here (the c.Res <= 0 cases returned above): bytes received
	// on this recv CQE, regardless of which buffer they landed in.
	w.bytesReadBatch += uint64(c.Res)

	// Past the first 250 ms of the shutdown's send drain an HTTP/1 conn is
	// read no more (celeris#806): the drain finishes the responses it has,
	// and a request taken now could only be answered into a queue that the
	// drain, which lasts as long as the budget, would never find empty
	// under steady keep-alive traffic. Epoll's loops have stopped reading
	// at the cancel. The bytes are dropped with the connection, which
	// shutdown closes; the recv is not re-armed. A detached conn is read no
	// more either (its writes were ended with the gate: stopDetachedProducers).
	// HTTP/2 conns are served on: they have had GOAWAY since the pool wait,
	// and what their responses may still wait for was waited for there or
	// is out of time.
	if w.refuseRecv && (!cs.detected || w.h1Only || engine.Protocol(cs.protocol.Load()) == engine.HTTP1) {
		if cqeHasBuffer(c.Flags) && w.bufRing != nil {
			w.bufRing.PushBuffer(cqeBufferID(c.Flags))
			w.hasBufReturns = true
		}
		return
	}

	// Direct-into-bodyBuf path: the previous recv SQE targeted
	// H1State.bodyBuf (NextRecvBuf). The CQE's Res applies to bodyBuf,
	// NOT cs.buf, so check this FIRST — indexing cs.buf[:c.Res] when
	// c.Res > cap(cs.buf) would panic. Skip ProcessH1, extend the body,
	// and dispatch the handler when the body is full.
	if cs.recvIntoBody && cs.h1State != nil {
		cs.recvIntoBody = false
		complete := cs.h1State.ConsumeBodyRecv(int(c.Res))
		if !complete {
			if !cqeHasMore(c.Flags) && !cs.recvLinked && !cs.recvPaused {
				if !w.prepareRecv(cs, w.pickRecvTarget(cs)) {
					cs.needsRecv = true
					w.markDirty(cs)
				}
			}
			return
		}
		rest, derr := cs.h1State.DispatchBufferedBody(cs.ctx, w.handler, cs.writeFn)
		if errors.Is(derr, conn.ErrUpgradeH2C) {
			if err := w.switchToH2(cs); err != nil {
				w.closeConn(fd)
				return
			}
			if mu := cs.detachMu; mu != nil {
				mu.Lock()
			}
			cs.recvLinked = false
			if w.bufRing == nil {
				if w.flushSendLink(cs) {
					w.markDirty(cs)
				}
			} else {
				if w.flushSend(cs) {
					w.markDirty(cs)
				}
			}
			if mu := cs.detachMu; mu != nil {
				mu.Unlock()
			}
			return
		}
		if derr != nil {
			w.closeConn(fd)
			return
		}
		if len(rest) > 0 {
			if perr := conn.ProcessH1(cs.ctx, rest, cs.h1State, w.handler, cs.writeFn); perr != nil {
				if !errors.Is(perr, conn.ErrHijacked) {
					w.closeConn(fd)
				}
				return
			}
		}
		// The direct-body tail: flushed unlinked (the next recv may target
		// the body buffer again), then a standalone recv — or none, held for
		// a hand-off (celeris#657). A refused write closes here too
		// (celeris#761).
		w.respondAndArm(cs, fd, c, false, true)
		return
	}

	var data []byte
	var providedBufID uint16
	hasProvidedBuf := false

	if cqeHasBuffer(c.Flags) && w.bufRing != nil {
		// Multishot recv with ring-mapped provided buffers.
		providedBufID = cqeBufferID(c.Flags)
		data = w.bufRing.GetBuffer(providedBufID, int(c.Res))
		hasProvidedBuf = true
	} else {
		// Per-connection buffer (single-shot recv).
		data = cs.buf[:c.Res]
	}

	// Auto protocol detection on first recv (no MSG_PEEK needed).
	if !cs.detected {
		// Detection needs at most detect.PrefaceLen bytes (the HTTP/2 client
		// preface): Detect decides on any input of that length or more, and
		// on fewer it is undecided only when the input is under MinPeekBytes
		// or starts "PRI " (any other input is HTTP/1 or unknown at once). So
		// what this conn can hold here is bounded by construction: under
		// PrefaceLen bytes, plus the recv in hand when a decision falls.
		//
		// A prefix that spans recvs (notably the 24-byte H2 client preface,
		// v1.5.0 review 2.8) is accumulated in cs.detectAccum, a Go-heap
		// buffer the kernel never targets: the single-shot path re-arms into
		// cs.buf at offset 0 and the bufRing path returns each provided
		// buffer, so there is no durable place for partial bytes but this.
		// The fast common case, detection resolving on the first recv, pays
		// no copy. Only the bytes Detect can look at are added before it
		// runs; the rest of the recv is appended once the verdict is known
		// to be a protocol this engine serves (below).
		detectData := data
		taken := 0
		if len(cs.detectAccum) > 0 {
			taken = min(len(data), detect.PrefaceLen-len(cs.detectAccum))
			cs.detectAccum = append(cs.detectAccum, data[:taken]...)
			detectData = cs.detectAccum
		}
		proto, derr := detect.Detect(detectData)
		if derr == detect.ErrInsufficientData && len(detectData) >= detect.PrefaceLen {
			// Unreachable while Detect decides at PrefaceLen bytes; kept so
			// that the bound does not rest on Detect's internals.
			derr = detect.ErrUnknownProtocol
		}
		if derr == detect.ErrInsufficientData {
			if len(cs.detectAccum) == 0 {
				// First partial recv: begin accumulating (fewer than
				// PrefaceLen bytes, as Detect has not decided).
				cs.detectAccum = append(cs.detectAccum, data...)
			}
			if hasProvidedBuf {
				// Provided-buffer bytes are now copied into detectAccum;
				// return the buffer to the kernel (early: we skip the normal
				// batch publish, P0).
				w.bufRing.ReturnBuffer(providedBufID)
			}
			if !cqeHasMore(c.Flags) {
				if !w.prepareRecv(cs, cs.buf) {
					cs.needsRecv = true
					w.markDirty(cs)
				}
			}
			return
		}
		if derr != nil {
			// ErrUnknownProtocol: these bytes are not a protocol this engine
			// serves, and more of them will not make them one. Close, as the
			// epoll engine does at the first unrecognised bytes. Re-arming
			// and waiting here let a client that opened with "PRI " keep the
			// connection, and every byte it sent, for as long as it liked
			// (celeris#974).
			cs.detectAccum = nil
			if hasProvidedBuf {
				w.bufRing.ReturnBuffer(providedBufID)
			}
			w.closeConn(fd)
			return
		}
		cs.protocol.Store(int32(proto))
		cs.detected = true
		cs.detectDeadline = 0
		w.initProtocol(cs)
		// If we accumulated across recvs, hand the FULL accumulated prefix
		// (not just this last recv) to the protocol handler below: the
		// accumulated bytes, then what of this recv Detect did not need.
		// data keeps the accumulator's array alive for the rest of this
		// function; the connState lets go of it, so a pooled connState
		// never holds the array of a conn that has gone.
		if len(cs.detectAccum) > 0 {
			data = append(cs.detectAccum, data[taken:]...)
			cs.detectAccum = nil
		}
	}

	// Async handler dispatch (Config.AsyncHandlers on HTTP1): hand the
	// received bytes to a per-conn goroutine and return to the CQE drain
	// immediately. The goroutine runs ProcessH1 under cs.detachMu and
	// enqueues on detachQueue so this worker submits SEND SQEs on its
	// own goroutine (SINGLE_ISSUER). Mirrors the epoll W3 shape.
	//
	// Per-handler async (celeris #300): only PROMOTED conns go straight
	// to the dispatch goroutine here. A fresh async-mode HTTP1 conn first
	// tries the inline fast path below (InlineMode); ProcessH1 bails with
	// ErrAsyncDispatch the moment it parses an async-marked route, at
	// which point the conn is promoted and its stashed request handed to
	// the goroutine. This lets sync routes run inline on the worker
	// (no handoff) on a server that mixes sync + async handlers.
	asyncFeed := false
	var hdrDL int64
	if w.async && cs.asyncPromoted.Load() && (w.h1Only || engine.Protocol(cs.protocol.Load()) == engine.HTTP1) {
		// Read the header deadline BEFORE the bytes reach the dispatch
		// goroutine: once fed, it may run the request, and an h2c upgrade
		// sets cs.h1State to nil there (celeris#722). Armed below.
		hdrDL = w.asyncHeaderDeadline(cs)
		cs.asyncInMu.Lock()
		// celeris#364: re-check under asyncInMu — the dispatch goroutine clears
		// asyncPromoted (reverting the conn to inline) under this same lock. If
		// it won the race, do NOT feed asyncInBuf (the goroutine is exiting);
		// fall through to inline with the unconsumed `data` instead.
		if cs.asyncPromoted.Load() {
			// #383 reverse (async): a new request raced a self-transplant. The
			// goroutine marked this conn for hand-off to epoll and exited, but
			// the client sent again first. ABORT the transplant (clear the flag
			// so drainDetachQueue skips it) and feed normally — `starting` below
			// is true (asyncRun==false), so we respawn the goroutine and the
			// request is served on io_uring. No request lost; retried next park.
			cs.transplantPending.Store(false)
			asyncFeed = true
		} else {
			cs.asyncInMu.Unlock()
		}
	}
	if asyncFeed {
		// Backpressure: drop the conn if the dispatch goroutine is
		// falling behind. Prevents a pipelining client from ballooning
		// asyncInBuf without bound.
		if len(cs.asyncInBuf)+len(data) > maxPendingInputBytes {
			cs.asyncInMu.Unlock()
			if hasProvidedBuf {
				w.bufRing.PushBuffer(providedBufID)
				w.hasBufReturns = true
			}
			w.closeConn(fd)
			return
		}
		// Append directly into asyncInBuf — dispatch goroutine swaps
		// with asyncOutBuf under the same mutex before running
		// ProcessH1, so the provided-buffer slice cannot be overwritten
		// in-flight. Zero allocation on steady state.
		cs.asyncInBuf = append(cs.asyncInBuf, data...)
		starting := !cs.asyncRun
		if starting {
			cs.asyncRun = true
		}
		cs.asyncInMu.Unlock()
		if hasProvidedBuf {
			w.bufRing.PushBuffer(providedBufID)
			w.hasBufReturns = true
		}
		// celeris#715 hypothesis (c), validation builds only: the same
		// window as promoteConnToAsync's (recvtheft.SetWakeHold).
		if recvtheft.Enabled {
			recvtheft.WakeHold()
		}
		if starting {
			w.asyncWG.Add(1)
			go w.runAsyncHandler(cs)
		} else {
			cs.asyncCond.Signal()
		}
		w.reqBatch++
		// Re-attempt the slowloris-defence timer arm if the initial
		// attempt (from initProtocol on accept) silently dropped under
		// SQ-ring pressure. ArmHeaderDeadline is idempotent on
		// HeaderDeadlineNs, so it won't reset the deadline; but
		// armHeaderTimer's headerTimerArmed gate means we only retry
		// the SQE submission if the prior arm is not in flight. This
		// is the explicit "retry on next recv" path that was missing —
		// without it, a conn whose initial arm failed relies entirely
		// on the sweep, doubling worst-case close latency on slow
		// refapps (observability/static_swagger_proxy). From the
		// deadline read before the feed, never from cs.h1State, which the
		// goroutine may be changing now (celeris#722).
		if hdrDL > 0 {
			w.armHeaderTimerAt(cs, hdrDL)
		}
		if !cqeHasMore(c.Flags) && !cs.recvPaused {
			if !w.prepareRecv(cs, cs.buf) {
				cs.needsRecv = true
				w.markDirty(cs)
			}
		}
		return
	}

	var processErr error
	// Stash the worker's cached "now" on H1State so populateCachedStream
	// can copy it to the stream — HandleStream skips a per-request
	// time.Now() vDSO call.
	if cs.h1State != nil {
		cs.h1State.NowNs = now
	}
	// Per-handler async (celeris #300): on an async-mode HTTP1 conn that
	// hasn't been promoted yet, run ProcessH1 inline in InlineMode so it
	// bails (ErrAsyncDispatch) when it hits an async route. The flag is
	// set only around the ProcessH1 call(s) below.
	tryInline := w.async && !cs.asyncPromoted.Load() && cs.h1State != nil &&
		(w.h1Only || engine.Protocol(cs.protocol.Load()) == engine.HTTP1)
	// h1Only mode (Protocol=HTTP1 + EnableH2Upgrade=false): no atomic
	// Load, no switch dispatch, no upgrade-handling block — ProcessH1
	// cannot return ErrUpgradeH2C because tryUpgradeH2C is gated off
	// in the parser. The compiler can specialize the call site without
	// hitting any of the H2 / upgrade machinery in the binary's hot
	// section.
	if w.h1Only {
		if tryInline {
			cs.h1State.InlineMode = true
		}
		processErr = conn.ProcessH1(cs.ctx, data, cs.h1State, w.handler, cs.writeFn)
		if tryInline {
			cs.h1State.InlineMode = false
		}
	} else {
		switch engine.Protocol(cs.protocol.Load()) {
		case engine.HTTP1:
			if tryInline {
				cs.h1State.InlineMode = true
			}
			processErr = conn.ProcessH1(cs.ctx, data, cs.h1State, w.handler, cs.writeFn)
			if tryInline {
				cs.h1State.InlineMode = false
			}
			if errors.Is(processErr, conn.ErrUpgradeH2C) {
				// H1→H2 upgrade. switchToH2 consumes the upgrade info and
				// re-arms recv so subsequent data is parsed as H2.
				if hasProvidedBuf {
					w.bufRing.PushBuffer(providedBufID)
					w.hasBufReturns = true
				}
				if err := w.switchToH2(cs); err != nil {
					w.closeConn(fd)
					return
				}
				// Flush the buffered 101 Switching Protocols + H2 server preface
				// + stream 1 response bytes. Without this explicit flush the
				// client blocks forever waiting for the 101 (the normal post-
				// process flush path below is bypassed by this early return).
				if mu := cs.detachMu; mu != nil {
					mu.Lock()
				}
				cs.recvLinked = false
				if w.bufRing == nil {
					if w.flushSendLink(cs) {
						w.markDirty(cs)
					}
				} else {
					if w.flushSend(cs) {
						w.markDirty(cs)
					}
				}
				if mu := cs.detachMu; mu != nil {
					mu.Unlock()
				}
				// Re-arm recv to keep reading H2 frames. flushSendLink may have
				// already chained a recv via IOSQE_IO_LINK — skip our standalone
				// re-arm in that case to avoid submitting two recv SQEs on the
				// same fd, which would split incoming H2 frames across CQEs and
				// occasionally lose END_STREAM delivery for later streams
				// (observed as flaky TestH2CUpgradeSubsequentStreams/iouring).
				if !cqeHasMore(c.Flags) && !cs.recvLinked {
					if !w.prepareRecv(cs, cs.buf) {
						cs.needsRecv = true
						w.markDirty(cs)
					}
				}
				return
			}
		case engine.H2C:
			// Async-mode conns: serialize inline ProcessH2 against the
			// runAsyncHandler goroutine that owns cs.writeBuf until its
			// H1→H2 upgrade-flush completes. Without the lock, a new
			// recv arriving while the goroutine is mid-flush runs
			// ProcessH2 → writeFn → cs.writeBuf manipulation concurrent
			// with the goroutine's `cs.writeBuf = cs.writeBuf[:0]`
			// clear — a data race matrixBenchStrict caught on the third
			// run (see issue #256 investigation thread).
			if cs.detachMu != nil {
				cs.detachMu.Lock()
				processErr = conn.ProcessH2(cs.ctx, data, cs.h2State, w.handler, cs.writeFn, w.h2cfg)
				cs.detachMu.Unlock()
			} else {
				processErr = conn.ProcessH2(cs.ctx, data, cs.h2State, w.handler, cs.writeFn, w.h2cfg)
			}
		}
	}

	// Per-handler async (celeris #300): handle the inline → dispatch
	// handoff. ProcessH1 (InlineMode) returned ErrAsyncDispatch the moment
	// it parsed an async-marked route; the request (+ any pipelined bytes)
	// is stashed in the H1 buffer. Promote the conn and hand the stashed
	// bytes to its dispatch goroutine, then return — every subsequent recv
	// goes straight to the dispatch path (asyncPromoted guard above).
	if tryInline {
		if errors.Is(processErr, conn.ErrAsyncDispatch) {
			// celeris#364: record the route that forced promotion (single-shot
			// recv only — the revert path assumes the worker-owned cs.buf recv
			// model). Set BEFORE asyncPromoted/goroutine start so the dispatch
			// goroutine observes it (happens-before). Empty path => the
			// goroutine treats the conn as not revert-eligible.
			//
			// CLONED, not aliased (celeris#631). CurrentRoute returns
			// h1.Request.Method/Path, and internPath/internMethod hand back
			// UnsafeString views INTO THE PARSER BUFFER — zero-copy, valid
			// only "while the H1 handler runs synchronously before the buffer
			// is reused" (internal/protocol/h1/intern.go). These two fields outlive
			// that window by design: they are read on every later park of the
			// dispatch goroutine, by which time cs.buf holds a LATER request.
			// Measured on the fixed stickiness rig: a conn promoted on "/db"
			// read its promotedPath back as "/cp" — the first three bytes of
			// the "/cpu" request that overwrote the buffer — so
			// canRevertToInline asked RouteAsync("GET", "/cp"), got false for
			// an unrouted path, and de-promoted a conn whose promoting route
			// is permanently async. The conn then re-promoted on the next /db
			// and the pair repeated once per request: 5 promotions across 12
			// requests on ONE connection instead of 1, the dispatch goroutine
			// spawned and joined each time, and the #364 revert deciding on
			// bytes that no longer belong to it.
			if w.bufRing == nil {
				method, path := cs.h1State.CurrentRoute()
				cs.promotedMethod, cs.promotedPath = strings.Clone(method), strings.Clone(path)
			}
			cs.asyncPromoted.Store(true)
			w.asyncPromoted.Add(1)
			stashed := cs.h1State.TakeBufferedBytes()
			// Flush any inline-handled response (pipelined sync request
			// before the async one) before promoting, so the sync
			// response ships immediately instead of waiting on the async
			// handler runtime (#300 L1). flushSend self-gates on
			// cs.sending / cs.zcNotifPending — when a SEND is already
			// in flight it's a no-op and the dispatch goroutine's later
			// flush picks up writeBuf intact, preserving order. SQ
			// pressure (returns true) is non-fatal: the dispatch
			// goroutine's direct unix.Write will still ship the bytes.
			if len(cs.writeBuf) > 0 && !cs.sending && !cs.zcNotifPending {
				_ = w.flushSend(cs)
			}
			if hasProvidedBuf {
				w.bufRing.PushBuffer(providedBufID)
				w.hasBufReturns = true
			}
			w.promoteConnToAsync(cs, fd, stashed, c)
			return
		}
		// Inline handled the request(s). If ProcessH1 left partial state
		// (buffered headers / accumulating body), promote so the
		// continuation runs on the dispatch goroutine — the partial-state
		// parse paths must not run inline (only the fresh-parse site
		// honors the async check).
		if processErr == nil && cs.h1State.HasPendingDispatchState() {
			// No complete request parsed yet (buffered headers / chunked), so
			// no route to record — leave promotedPath empty: not revert-eligible.
			cs.asyncPromoted.Store(true)
			w.asyncPromoted.Add(1)
		}
	}

	// Batch-return the provided buffer after processing. The data has been
	// consumed by the protocol handler. Actual publish happens after the CQE
	// drain loop completes (P0).
	//
	// Not for a hijacked request (celeris#868): the handler keeps the strings
	// it read before Hijack, views of this buffer, for the goroutine that
	// serves the connection, and a buffer pushed back is written by the next
	// receive of any connection of this worker. Retire it: it stays with the
	// hijacker, and the ring gets a fresh entry in its place. The same
	// guarantee the single-shot receive path has, whose connState (and so
	// receive buffer) a hijack drops instead of recycling (celeris#733).
	if hasProvidedBuf {
		if processErr != nil && errors.Is(processErr, conn.ErrHijacked) {
			w.bufRing.RetireBuffer(providedBufID)
		} else {
			w.bufRing.PushBuffer(providedBufID)
		}
		w.hasBufReturns = true
	}

	w.reqBatch++

	// Slowloris-defence: retry timer arm if the initial attempt dropped
	// silently under SQ-ring pressure. Mirrors the async-path retry above.
	// Sync mode: ProcessH1 is idempotent on HeaderDeadlineNs so it won't
	// re-trigger OnHeaderDeadlineArmed if the deadline is already set,
	// leaving conns whose initial arm failed dependent on the sweep alone.
	if cs.h1State != nil && cs.h1State.HeaderDeadlineNs.Load() > 0 && !cs.headerTimerArmed {
		w.armHeaderTimer(cs)
	}

	// lastActivity already set above; timeout checked in checkTimeouts.

	if processErr != nil {
		if errors.Is(processErr, conn.ErrHijacked) {
			return // FD already detached
		}
		// Flush pending writes (e.g. error responses) before closing, and
		// read cs.h1State, both under detachMu.
		//
		// The flush was previously outside the lock. completeSend's docstring
		// states the invariant it broke: for a detached connection cs.sendBuf
		// is read by the makeWriteFn closure on the user's goroutine, so every
		// mutation of it needs detachMu. flushSend swaps sendBuf with writeBuf
		// and sets cs.sending, so calling it unlocked races a handler that is
		// writing — the same access pattern the race detector caught on the
		// call sites that were fixed when that docstring was written
		// (celeris#528). Reachable within one request: a handler can Detach
		// (SSE, or a WebSocket upgrade) and then have ProcessH1 surface an
		// error, with the guarded write closure already installed.
		//
		// The h1State read has its own reason to be under the lock: the async
		// dispatch goroutine's switchToH2Local nils cs.h1State under the same
		// mutex (the #256 TOCTOU class).
		//
		// internal/engine/epoll/loop.go does exactly this — one lock spanning the
		// flush and OnError.
		if mu := cs.detachMu; mu != nil {
			mu.Lock()
			_ = w.flushSend(cs)
			if cs.h1State != nil && cs.h1State.OnError != nil {
				cs.h1State.OnError(processErr)
			}
			mu.Unlock()
		} else {
			_ = w.flushSend(cs)
		}
		w.closeConn(fd)
		return
	}

	// Flush response with linked RECV when using single-shot per-connection
	// buffers. The linked SEND→RECV lets the kernel start RECV immediately
	// after SEND completes, eliminating one loop iteration per request.
	cs.recvLinked = false
	w.respondAndArm(cs, fd, c, true, true)
}

// respondAndArm is the one tail every H1 request path in handleRecv takes
// once the request has produced its response (celeris#657): flush the
// response, then arm the connection's next recv, chained behind the SEND
// (link, single-shot recv only; flushSendLink falls back to an unlinked send
// itself when it must) or standalone. capCheck applies the back-pressure
// close of the sync tail: a conn whose write hooks refused bytes is closed,
// and its close sends what was staged first. A backlog over the cap by
// itself is not a reason to close: one response larger than the cap puts it
// there, and so does an HTTP/2 stream's flow-control window; closing on it
// cut such a response off (celeris#761).
//
// With a drain set and a conn the hand-off would take, the response is
// flushed UNLINKED and no recv is armed at all (HOLD): the conn is handed off
// at that SEND's completion with nothing in the kernel, or releaseHold arms
// the recv there if it is not. With no drain set it places exactly the SQEs
// the two tails placed before they shared it
// (TestNoDrainSQESequenceIsUnchanged).
func (w *Worker) respondAndArm(cs *connState, fd int, c *completionEntry, link, capCheck bool) {
	// Hoist the detachMu load: the same mu is checked Lock and Unlock on
	// the success path, plus once on the early-return overflow path.
	mu := cs.detachMu
	if mu != nil {
		mu.Lock()
	}
	// Back-pressure: read writeRefused inside the lock so concurrent
	// goroutine writes via the guarded writeFn don't race the read.
	if capCheck && cs.writeRefused {
		if mu != nil {
			mu.Unlock()
		}
		w.closeConn(fd)
		return
	}
	hold := w.bufRing == nil && w.transplant.Load() != nil && w.holdEligible(cs)
	switch {
	case hold:
		cs.transplantHold = true
		w.handoffLoss.noteHeld()
		if w.flushSend(cs) {
			w.markDirty(cs)
		}
	case link && w.bufRing == nil:
		if w.flushSendLink(cs) {
			w.markDirty(cs)
		}
	default:
		if w.flushSend(cs) {
			w.markDirty(cs)
		}
	}
	if mu != nil {
		mu.Unlock()
	}
	if hold {
		return
	}

	// For multishot recv, CQE_F_MORE means the kernel will produce more CQEs
	// without needing a new SQE. Only re-arm if multishot ended.
	// For linked SEND→RECV, the RECV is already queued — skip standalone re-arm.
	// Don't re-arm if recv is paused (WebSocket backpressure).
	if !cqeHasMore(c.Flags) && !cs.recvLinked && !cs.recvPaused {
		if !w.prepareRecv(cs, w.pickRecvTarget(cs)) {
			cs.needsRecv = true
			w.markDirty(cs)
		}
	}
}

func (w *Worker) handleSend(c *completionEntry, fd int, now int64) {
	cs := w.conns[fd]
	if cs == nil {
		return
	}
	// The completion is applied under detachMu when the conn has one: the
	// dispatch goroutine and a detached conn's guarded writeFn read the send
	// state under it. Every branch below releases it. When the goroutine
	// holds it across a handler, the completion is held instead
	// (celeris#750); see holdOrLockSend.
	mu := cs.detachMu
	if mu != nil && !w.holdOrLockSend(cs, c) {
		return
	}

	// SEND_ZC notification CQE: the NIC has finished DMA-reading the buffer.
	// Now safe to modify/reuse sendBuf. Process the deferred result.
	if cqeIsNotif(c.Flags) {
		// celeris#591: one atomic per NOTIF. Reached only on the ZC path —
		// a plain SEND never produces a CQE_F_NOTIF completion.
		w.zc.noteNotif()
		validation.IouringSendZCNotifs.Add(1)
		// zcNotifPending is read by the inline-egress guard on the dispatch
		// goroutine under detachMu, which is held here.
		//
		// celeris#591: the NOTIF is the instant the guard reopens. If
		// writeBuf already holds queued bytes, the very next inline
		// unix.Write is admitted against data the worker has not yet
		// flushed — the ordering celeris#587 exercises. Read under
		// detachMu, the same lock the dispatch goroutine writes it
		// under, so this witness is not itself a race.
		if len(cs.writeBuf) > 0 {
			validation.IouringZCCompletionWithPendingWrite.Add(1)
		}
		cs.zcNotifPending = false
		closeAfter := w.completeSend(cs, fd, int(cs.zcSentBytes), now, true)
		if mu != nil {
			mu.Unlock()
		}
		if closeAfter {
			w.closeConn(fd)
		}
		return
	}

	// SEND_ZC first CQE: result is ready but buffer is still in DMA.
	// Store the result and wait for the notification before touching sendBuf.
	//
	// Keyed on IORING_CQE_F_MORE ALONE, never on w.sendZC. How a completion
	// must be interpreted is fixed by how its SQE was submitted, and
	// w.sendZC is mutable: the EINVAL/ENOMEM fallbacks flip it to false
	// while ZC sends are still in flight. Gating this branch on it meant
	// every in-flight ZC send's first CQE fell through to completeSend,
	// which cleared cs.sending and re-flushed -- and then the real
	// notification arrived and ran completeSend a SECOND time against a
	// stale cs.zcSentBytes, clearing cs.sending for a send that was
	// genuinely in flight. The connection ends up with cs.sending stuck
	// true and nothing to clear it, and the dirty-list flush skips any
	// conn with cs.sending set, so it is stranded for the life of the
	// process (celeris#519). F_MORE on a udSend completion is set by the
	// kernel only for SEND_ZC, so it is the accurate test.
	if cqeHasMore(c.Flags) {
		// cs.sending / cs.zcNotifPending are read by the inline-egress guard on
		// the dispatch goroutine under detachMu; mutate them under the lock
		// (held).
		if mu != nil {
			defer mu.Unlock()
		}
		if c.Res < 0 {
			cs.sending = false
			cs.zcNotifPending = true
			cs.zcSentBytes = c.Res // store negative for error path on NOTIF
			return
		}
		cs.zcNotifPending = true
		cs.zcSentBytes = c.Res
		// sending stays true until NOTIF completes the cycle.
		return
	}

	// Regular SEND completion (non-ZC path). cs.sending is reset on
	// every exit branch — by completeSend on success, inline on error
	// paths — so we skip the entry-reset to save one write per request
	// on the hot success path.

	// SEND_ZC fallback. EINVAL: the kernel does not support the opcode.
	// ENOMEM: SEND_ZC pins the user buffer against RLIMIT_MEMLOCK, and a
	// host with a low limit returns ENOMEM once enough sends are in flight
	// -- the same limit that caps the worker count on a CI runner. Both are
	// reasons to stop using ZC on this worker, NOT reasons to close a
	// healthy connection, which is what the celeris#519 reproduction shows
	// happening. Disable ZC and retry the send with a regular SEND.
	//
	// Gated on cs.sendIsZC, the provenance of THIS completion, never on
	// w.sendZC (celeris#609). w.sendZC is cleared by the first fallback on
	// the worker, so keying the classification on it absorbed exactly one
	// failure and sent every ZC send still in flight behind it down the
	// generic c.Res < 0 path below -- OnError(-ENOMEM) then closeConn, a
	// healthy connection torn down by a transient resource shortage. This
	// is the same correction already made thirty lines above for the
	// CQE_F_MORE branch selection.
	if cs.sendIsZC && (c.Res == -int32(unix.EINVAL) || c.Res == -int32(unix.ENOMEM)) {
		w.retireSendZC(-c.Res, "SEND_ZC unavailable, falling back to regular SEND")
		// cs.sending is read by the inline-egress guard under detachMu; clear it
		// and re-flush under the lock (held; flushSend for a detached conn is
		// always called under detachMu, as in the dirty-flush loop).
		cs.sending = false
		if w.flushSend(cs) {
			w.markDirty(cs)
		}
		if mu != nil {
			mu.Unlock()
		}
		return
	}

	if c.Res < 0 {
		// A peer that left before the response flushed gets its own
		// bucket (celeris#645): it is one count per abandoned request,
		// not an engine fault, and it is the bucket an io_uring column
		// carries while the epoll column of the same refapp counts
		// nothing at all.
		w.errs.SendFailed(unix.Errno(-c.Res))
		// cs.sending / cs.sendBuf are read by the inline-egress guard under
		// detachMu; reset them (and writeBuf) inside the lock (held) rather than
		// before it, so the dispatch-goroutine read never races this error
		// completion.
		cs.sending = false
		cs.sendBuf = cs.sendBuf[:0]
		cs.writeBuf = cs.writeBuf[:0]
		if cs.h1State != nil && cs.h1State.OnError != nil {
			cs.h1State.OnError(errIORingSend(c.Res))
		}
		if mu != nil {
			mu.Unlock()
		}
		if cs.closing {
			w.finishCloseAny(fd, cs)
		} else {
			w.closeConn(fd)
		}
		return
	}

	closeAfter := w.completeSend(cs, fd, int(c.Res), now, cs.sendIsZC)
	if mu != nil {
		mu.Unlock()
	}
	if closeAfter {
		w.closeConn(fd)
	}
}

// holdOrLockSend takes cs.detachMu for send completion c and reports true,
// or holds c on cs (heldSends) and reports false. Worker thread.
//
// The dispatch goroutine holds detachMu across ProcessH1, i.e. for as long as
// the user handler runs, and a blocking Lock here parked the LockOSThread'd
// worker, and every connection of its ring, until the handler returned
// (celeris#750, the fifth site of the celeris#704 class). The common shape is
// a pipelining client: the rest of a response goes out as a ring SEND, and
// the next request's handler is running when the client reads it. The
// completion cannot be skipped, since its result must be applied, so it is
// held, and the goroutine owes the conn back (relinkOwed, set in the same
// asyncInMu section by dispatchBusy); replayHeldSends applies it then.
//
// A completion is also held, whatever the lock, while an earlier one of the
// conn is: they are applied in arrival order (a SEND_ZC's notification after
// its first completion). When the goroutine is parked or gone, whoever holds
// the lock holds it for one write, and waiting for it is bounded, as it
// always was; see dispatchBusy.
func (w *Worker) holdOrLockSend(cs *connState, c *completionEntry) bool {
	if len(cs.heldSends) == 0 {
		if cs.detachMu.TryLock() {
			return true
		}
		if !dispatchBusy(cs, &cs.relinkOwed) {
			cs.detachMu.Lock()
			return true
		}
	}
	cs.heldSends = append(cs.heldSends, *c)
	return false
}

// replayHeldSends applies cs's held send completions (celeris#750) through
// handleSend, in arrival order. It runs on the worker thread wherever the
// conn is next acted on: drainDetachQueue, for the dispatch goroutine's
// hand-back (any entry of the conn), and closeConn, so that a close deferred
// behind cs.sending never waits for a completion that has already arrived.
// If the goroutine holds detachMu across a handler again, handleSend holds
// that completion again, and every later one behind it, and the hand-back is
// owed again. The CQE dispatch's post-send hand-off attempt (tryTransplant)
// is not repeated: a conn whose goroutine lives is that goroutine's to claim,
// and a claimed one is finished by the drain entry this runs from.
func (w *Worker) replayHeldSends(cs *connState) {
	fd := cs.fd
	held := cs.heldSends
	// A completion held again is appended to the front of held's array, at
	// an index no later than the one being applied: the loop has read it.
	cs.heldSends = held[:0]
	for i := range held {
		c := held[i]
		// A conn that left its slot since (its completions were dispatched
		// for it) has nothing left to apply them to.
		if fd < 0 || fd >= len(w.conns) || w.conns[fd] != cs || decodeGen(c.UserData) != cs.generation {
			cs.heldSends = cs.heldSends[:0]
			return
		}
		w.handleSend(&c, fd, w.cachedNow)
	}
}

// retireSendZC handles a send completion that failed for a reason that
// condemns the SEND_ZC opcode rather than the connection: EINVAL (the kernel
// does not support it) or ENOMEM (SEND_ZC pins the user buffer against
// RLIMIT_MEMLOCK, and a host with a low limit returns ENOMEM once enough
// sends are in flight).
//
// It separates the two things the old inline `w.sendZC = false` conflated
// (celeris#609). Turning the opcode off and telling the operator about it is
// a ONCE-per-worker event, so both are guarded on w.sendZC still being set.
// Deciding that the completion was survivable is a PER-COMPLETION judgement,
// so it is the caller's branch condition and happens every time, including
// for every ZC send that was already in flight when the first failure
// cleared the flag. The counter is the witness that the second group exists:
// under the defect it was exactly the set of connections that got closed.
func (w *Worker) retireSendZC(errno int32, msg string) {
	validation.IouringSendZCFallbacks.Add(1)
	if !w.sendZC {
		return
	}
	w.sendZC = false
	if w.logger != nil {
		w.logger.Warn(msg, "worker", w.id, "err", errno)
	}
}

// completeSend processes a send result after the buffer is safe to modify.
// Called directly for regular SEND, or from the NOTIF handler for SEND_ZC.
//
// For detached connections, cs.sendBuf is read by the makeWriteFn closure
// running on the user's goroutine (back-pressure check at line 1222).
// All cs.sendBuf mutations below MUST be guarded by detachMu when one
// exists, otherwise the goroutine read races the event-loop write —
// observed via -race in TestNativeEngineLargePayload/io_uring.
// Reports whether the CALLER must close the connection. closeConn takes
// cs.detachMu, and this function runs under that same lock (its caller,
// handleSend, holds it for the whole body); sync.Mutex is not reentrant, so
// closing inline wedges the worker thread
// against itself -- and with it the entire event loop: no CQE is processed,
// the detach queue is never drained (so every WebSocket recv-pause the
// middleware asked to lift stays paused), no timeout sweep runs, and
// graceful shutdown never completes. Releasing the lock early instead is
// NOT the fix: it opens the window the lock exists to close, and measurably
// corrupts streams (protoErr 0 -> 47 on the celeris#519 reproduction). The
// caller closes once it has released the lock.
//
// A multi-worker engine only loses the one worker to this, so its
// connections hang while the others keep serving; on a single-worker engine
// -- what RLIMIT_MEMLOCK forces on a CI runner -- it takes the server down.
// fromZC says whether the completion being processed belongs to a SEND_ZC
// SQE. It is the caller's knowledge, not a re-derivation: the NOTIF handler
// passes true because a CQE_F_NOTIF completion is produced by nothing else,
// and the plain-SEND call site passes cs.sendIsZC, the provenance recorded
// when that SQE was armed. Never w.sendZC (celeris#609).
func (w *Worker) completeSend(cs *connState, fd int, sent int, now int64, fromZC bool) (closeAfter bool) {
	// The caller (handleSend) holds detachMu for detached and async
	// connections, so the entire state mutation (cs.sending clear / sendBuf
	// truncate / writeBuf reset / OnError fire) is serialized against the
	// goroutine writeFn path. The inline-egress fast path (the initProtocol
	// guarded closure) reads cs.sending under detachMu to decide whether a
	// ring SEND is in-flight, so the clear MUST be inside the lock — otherwise
	// that read races this completion.
	cs.sending = false
	// celeris#607 witness. cs.recvLinked is cleared only when the chained
	// recv's own CQE is processed, and that recv cannot run before this
	// send completes — so reaching here with it still set means the recv
	// was queued behind this send for the whole interval, and that is how
	// long the connection was unable to read.
	//
	// `now` is the loop's cached clock, not a fresh vDSO read, and the
	// atomics are behind linkBlockedFloorNanos. A healthy request/response
	// send completes inside one loop pass, so the hot path pays one
	// subtract and one compare and never reaches the counters; only a send
	// that actually held its recv for a millisecond or more is recorded.
	if cs.recvLinked && cs.linkArmedAt != 0 {
		blocked := now - cs.linkArmedAt
		cs.linkArmedAt = 0
		if blocked >= linkBlockedFloorNanos {
			w.recvArm.noteLinkedRecvBlocked(blocked)
			linkedRecvProbe(cs, blocked)
		}
	}

	if fromZC && (sent == -int(unix.ENOMEM) || sent == -int(unix.EINVAL)) {
		// SEND_ZC pins the user buffer against RLIMIT_MEMLOCK. A host with
		// a low limit returns ENOMEM once enough sends are in flight --
		// the same limit that caps the worker count on a CI runner
		// ("io_uring workers capped by RLIMIT_MEMLOCK ... capped_to=1").
		// That is a transient resource shortage, not a broken connection:
		// closing here drops healthy connections mid-stream, which is
		// exactly what the celeris#519 reproduction shows (16 conns killed
		// with -12 in one 40 s run). Fall back to regular SEND for this
		// worker and re-flush, mirroring the EINVAL/unsupported path.
		//
		// cs.sending was cleared above and cs.sendBuf still holds the
		// unsent bytes, so flushSend re-issues them without ZC.
		//
		// celeris#609: gated on fromZC -- the provenance of this very
		// completion -- and NOT on w.sendZC, which the first fallback on
		// the worker has already cleared. Under the old `&& w.sendZC`
		// guard the first ENOMEM was absorbed and every zero-copy send
		// already in flight behind it fell through to the `sent < 0` path
		// below, which fires OnError(-ENOMEM) and tells handleSend to
		// closeConn. EINVAL rides the same branch for the same reason it
		// does in handleSend: it is a reason to stop using the opcode, not
		// a reason to kill the connection. The retry is armed by flushSend
		// with w.sendZC already false, so it is a plain SEND and a second
		// failure takes the generic error path -- exactly one retry.
		w.retireSendZC(int32(-sent), "SEND_ZC notification reported failure, falling back to regular SEND")
		if w.flushSend(cs) {
			w.markDirty(cs)
		}
		return false
	}

	if sent < 0 {
		w.errs.SendFailed(unix.Errno(-sent))
		cs.sendBuf = cs.sendBuf[:0]
		cs.writeBuf = cs.writeBuf[:0]
		cs.sendBody = nil
		cs.bodyBuf = nil
		if cs.h1State != nil && cs.h1State.OnError != nil {
			cs.h1State.OnError(errIORingSend(int32(sent)))
		}
		if cs.closing {
			// finishCloseAny only READS cs.detachMu (to pick the detached
			// variant); it never takes it, so it is safe under the lock.
			w.finishCloseAny(fd, cs)
			return false
		}
		return true
	}

	// sent >= 0 here: payload bytes flushed by this send completion
	// (covers regular SEND and the SEND_ZC NOTIF path, both of which
	// reach completeSend with the byte count).
	w.bytesWrittenBatch += uint64(sent)
	if cs.closing && sent > 0 {
		// The closing drain's clock (closingDrainBound) measures how long
		// the peer has taken nothing, not how long the drain has run: a
		// response larger than the socket buffers to a client that reads
		// it steadily is not cut off (celeris#761).
		cs.lastActivity = now
	}
	// celeris#591: the ring-send share of those bytes. Plain local add on
	// the per-request send path — it is published with one atomic per
	// event-loop iteration next to bytesWrittenBatch, never per request.
	w.ringBytesBatch += uint64(sent)

	// Partial-send handling, split by whether we issued a plain SEND
	// (sendBuf only) or a WRITEV (sendBuf + sendBody). Partial WRITEV
	// responses collapse the remainder into sendBuf so the retry path
	// uses the plain SEND fast-path; this pays a one-time body-sized
	// memcpy only when the kernel returned a short send (rare on
	// localhost TCP, occasional on congested networks).
	if len(cs.sendBody) > 0 {
		headerLen := len(cs.sendBuf)
		total := headerLen + len(cs.sendBody)
		switch {
		case sent >= total:
			cs.sendBuf = cs.sendBuf[:0]
			cs.sendBody = nil
		case sent >= headerLen:
			cs.sendBuf = cs.sendBuf[:0]
			cs.sendBuf = append(cs.sendBuf, cs.sendBody[sent-headerLen:]...)
			cs.sendBody = nil
		default:
			remaining := headerLen - sent
			copy(cs.sendBuf, cs.sendBuf[sent:])
			cs.sendBuf = cs.sendBuf[:remaining]
			cs.sendBuf = append(cs.sendBuf, cs.sendBody...)
			cs.sendBody = nil
		}
	} else if sent < len(cs.sendBuf) {
		remaining := len(cs.sendBuf) - sent
		copy(cs.sendBuf, cs.sendBuf[sent:])
		cs.sendBuf = cs.sendBuf[:remaining]
	} else {
		cs.sendBuf = cs.sendBuf[:0]
	}
	// detachMu (if any) is held by the caller for the whole function — no
	// per-branch Unlock needed below.
	if cs.closing && len(cs.sendBuf) == 0 && len(cs.writeBuf) == 0 {
		w.finishCloseAny(fd, cs)
		return
	}

	// All data sent — re-arm recv if needed, remove from dirty list.
	if len(cs.sendBuf) == 0 && len(cs.writeBuf) == 0 {
		if cs.needsRecv && !cs.recvPaused {
			if w.prepareRecv(cs, cs.buf) {
				cs.needsRecv = false
				w.endRecvStall(cs)
			} else {
				w.markDirty(cs)
				return
			}
		}
		w.removeDirty(cs)
		cs.lastActivity = now
		return
	}

	// Re-send remainder or flush new data. Only markDirty on SQ ring full.
	// detachMu (if any) is held by the caller.
	if w.flushSend(cs) {
		w.markDirty(cs)
	}
	return false
}

func (w *Worker) handleClose(fd int) {
	// finishClose already removed from conns and decremented activeConns.
	// With CQE_SKIP_SUCCESS, this handler may not fire for successful close.
	// Clear the slot as a safety guard for error CQEs.
	if fd >= 0 && fd < len(w.conns) {
		w.conns[fd] = nil
	}
	// Note: liveConns removal is the caller's responsibility — every
	// path that calls finishCloseAny reaches here through closeConn or
	// a similar path that already removed the FD from liveConns.
}

func (w *Worker) closeConn(fd int) {
	cs := w.conns[fd]
	if cs == nil {
		return
	}
	// A send completion held for the dispatch goroutine (celeris#750) is
	// applied first: the close below defers itself behind cs.sending, and
	// the completion that would end that wait has already arrived. If the
	// goroutine still holds detachMu across a handler, it stays held, and so
	// does the close (closeOwed, below).
	if len(cs.heldSends) > 0 {
		closing := cs.closing
		w.replayHeldSends(cs)
		if w.conns[fd] != cs || cs.closing != closing {
			return // the completion closed it, or deferred its close
		}
	}
	detached := cs.detachMu != nil
	if detached {
		cs.asyncClosed.Store(true)
		if cs.asyncCond.L != nil {
			cs.asyncInMu.Lock()
			cs.asyncCond.Broadcast()
			cs.asyncInMu.Unlock()
		}
		// Signal the detached goroutine's writeFn to stop writing. The
		// mutex serializes with any in-progress write: if the goroutine is
		// mid-write, we wait until it finishes.
		//
		// But not for a handler (celeris#704, the io_uring twin of
		// celeris#669). The dispatch goroutine holds this mutex across
		// ProcessH1, i.e. for as long as the user handler runs, and a
		// blocking Lock here parked the LockOSThread'd worker, and every
		// connection of its ring (no CQE processed, no accept, no flush),
		// until the handler returned: through the recv FIN and error
		// branches and every other close of a conn whose handler still runs.
		// When the lock is held and that goroutine is running, leave the
		// close to it: asyncClosed is set, so it exits at its next check, and
		// its exit hands cs back through the detach queue, whose asyncClosed
		// branch calls here again with the lock free. Until then the conn
		// stays whole (in the table, the live set, its descriptor open)
		// because the handler is still writing its response into it. When the
		// goroutine is parked or gone, the holder is a guarded writeFn in one
		// write, and waiting for it is bounded; see dispatchBusy.
		if !cs.detachMu.TryLock() {
			if dispatchBusy(cs, &cs.closeOwed) {
				return
			}
			cs.detachMu.Lock()
		}
		// A recv branch that met a running handler left its error here
		// rather than wait for the lock (closeErr, celeris#704). Deliver it
		// as that branch did, under the lock, before the close.
		if err := cs.closeErr; err != nil {
			cs.closeErr = nil
			if cs.h1State != nil && cs.h1State.OnError != nil {
				cs.h1State.OnError(err)
			}
		}
		// celeris#549 window (celeris#584 exposure counter): OnDetach has
		// published the detach (asyncDetachPending set on the dispatch
		// goroutine, or inline on this thread in async mode) but the
		// deferred increment in drainDetachQueue has not run, and it never
		// will for this conn because the drain skips on detachClosed. Read
		// under detachMu — the flag is written under it on the promoted
		// path — and only on the first close of the conn.
		windowClose := !cs.detachClosed && cs.asyncDetachPending && !cs.detachCounted
		cs.detachClosed = true
		// Acquire barrier: only invoke OnDetachClose once the WS upgrade has
		// fully wired the conn (WSReady). Otherwise the read of OnDetachClose —
		// and the ws.Close() it calls — races the upgrade installing it and the
		// rest of the ws state on the async goroutine after Detach released
		// detachMu (peer RST mid-upgrade). Not-yet-wired conns are still torn
		// down via the fd close + read path below.
		if cs.h1State != nil && cs.h1State.WSReady.Load() && cs.h1State.OnDetachClose != nil {
			cs.h1State.OnDetachClose()
			cs.h1State.OnDetachClose = nil
		}
		cs.detachMu.Unlock()
		// Drop callbacks once the engine relinquishes the conn so any late
		// goroutine references resolve to no-ops without crashing. Same acquire
		// barrier as OnDetachClose above: only drop them once the WS upgrade has
		// finished reading them (via WSReadPauser) and published WSReady — before
		// that, the async upgrade goroutine is still reading PauseRecv/ResumeRecv.
		if cs.h1State != nil && cs.h1State.WSReady.Load() {
			cs.h1State.PauseRecv = nil
			cs.h1State.ResumeRecv = nil
		}
		// Only decrement when OnDetach actually fired (WS/SSE detach).
		// Async mode pre-allocates detachMu in acquireConnState but does
		// NOT increment detachedCount, so decrementing here would cause
		// underflow for plain async-HTTP1 conns.
		if windowClose && w.detachWindowCloses != nil {
			w.detachWindowCloses.Add(1)
		}
		w.releaseDetachedCount(cs)
	}
	w.removeDirty(cs)
	// Close H1 state unless a real WS/SSE detach handed ownership to a
	// middleware goroutine. Async-mode HTTP1 conns have detachMu set
	// but h1State.Detached is false — we still own H1 state there.
	//
	// For detached / async-dispatched conns, we MUST hold detachMu
	// while tearing down h1State. runAsyncHandler runs ProcessH1 under
	// the same lock, and CloseH1 writes state fields (state.stream,
	// state.bodyBuf, state.bodyNeeded) that ProcessH1 reads. Without
	// the lock, a peer close arriving while the goroutine is mid-
	// ProcessH1 races the h1State teardown; under churn-close+
	// async+auto+upg the resulting memory corruption manifested as a
	// SIGSEGV in runtime.stackpoolalloc after ~16 h of load
	// (#256). cs.asyncClosed was already set earlier in this function
	// and the goroutine checks it on loop re-entry, and a close that
	// found it inside a handler returned above (celeris#704), so
	// acquiring detachMu here only waits out a bounded holder: the
	// goroutine's own asyncClosed re-check, or a guarded write.
	trulyDetached := detached && cs.h1State != nil && cs.h1State.Detached.Load()
	if !trulyDetached && cs.h1State != nil {
		if detached {
			cs.detachMu.Lock()
			// Re-read Detached UNDER the lock (mirrors epoll e100873). An
			// in-flight OnDetach publishes Detached before releasing detachMu,
			// so if the conn detached while we waited the WS/SSE middleware
			// goroutine owns teardown — skip CloseH1; else we recycle a
			// Context/stream it is still using (the use-after-recycle that
			// nil-derefs WSRawWriteFn under a peer RST mid-upgrade).
			if !cs.h1State.Detached.Load() {
				conn.CloseH1(cs.h1State)
			}
			cs.detachMu.Unlock()
		} else {
			conn.CloseH1(cs.h1State)
		}
	}
	if cs.h2State != nil {
		conn.CloseH2(cs.h2State)
		w.removeH2Conn(fd)
	}

	// Defer actual close until all in-flight and pending SENDs complete,
	// so the last bytes (GOAWAY / RST_STREAM / WS close-echo) reach the
	// client before SHUT_WR. For detached connections this specifically
	// guards the WS close handshake: the WS middleware queues a close-
	// echo frame and then asks the engine to drop the FD via
	// SetWSIdleDeadline(1); without this guard the subsequent
	// checkTimeouts → closeConn pair would fire before the SEND SQE
	// submitted by the dirty-list flush has completed, and the echo
	// would never leave the kernel.
	if cs.sending || cs.zcNotifPending || len(cs.sendBuf) > 0 || len(cs.writeBuf) > 0 {
		cs.closing = true
		// lastActivity is the deadline base for checkTimeouts'
		// closingDrainTimeoutNanos sweep, so restamp it here: the close we are
		// deferring is most often one the sweep itself just triggered (idle /
		// read / WS-idle deadline), which means lastActivity is already hours
		// stale and the next sweep would reap the conn before the bytes below
		// could reach the kernel — killing the very flush this branch exists
		// for. cachedNow (not time.Now) keeps the churn-close close path free
		// of a vDSO call; its ~100 ms staleness is noise against a 5 s bound.
		cs.lastActivity = w.cachedNow
		if w.flushSend(cs) {
			w.markDirty(cs)
		}
		return
	}

	if detached {
		// Skip deferred-close and pool return while any goroutine holds
		// closure references to cs (WS/SSE detach or async dispatch).
		// GC collects cs once the goroutine finishes and all closure
		// references are dropped.
		w.finishCloseDetached(fd, cs)
		return
	}

	w.finishClose(fd)
}

// cancelConnOps submits ASYNC_CANCEL SQEs for every conn-buffer-targeting
// op the kernel still holds for cs — the armed recv and (belt-and-braces;
// the close paths drain sends first) an in-flight send. Called by the
// close paths BEFORE the fd is closed: unix.Close does not complete a
// pending io_uring recv, so without the cancel the op would sit armed on
// cs.buf until straggler data (e.g. a retransmitted POST segment at
// RTO ≥ 200 ms) completes it — the v1.4.15/7beebb9 heap-corruption trigger. With the
// cancel, the terminal CQE (-ECANCELED, or the op's natural completion if
// it raced the cancel) arrives within a loop pass or two and
// drainPendingRelease can release cs promptly.
//
// Targeting mirrors prepCancelUserDataSkipSuccess's WS-pause usage: match
// by the op's exact generation-tagged user_data, which is unambiguous for
// both real fds and fixed-file indexes, and — because the cancel SQE
// enters the ring BEFORE the fd/slot can be reused — can never hit a
// successor conn's op. The cancel's own CQE is suppressed on success and
// tagged udProvide on failure (-ENOENT/-EALREADY when the op completed
// first), so the dispatcher drops it; only the cancelled op's own
// terminal CQE feeds the kernelInflight accounting.
//
// On a full SQ ring, mirror armHeaderTimer: Submit to drain, retry once,
// and otherwise proceed without the cancel and report it (missed): a recv or
// send is left armed that nothing has asked the kernel to end. The close
// paths then shut the socket down (an owed recv ends when it is issued),
// mark cs (cancelMissed), and drainPendingRelease places the cancel again on
// its next passes and, past the backstop, holds cs for the op instead of
// releasing it (celeris#869). A hijack cannot shut the socket down, so it
// refuses instead (hijackConn).
func (w *Worker) cancelConnOps(fd int, cs *connState) (missed bool) {
	missed = !w.cancelRecvOp(fd, cs)
	return w.cancelOtherOps(fd, cs) || missed
}

// cancelRecvOp places the cancel of cs's armed recv, if it has one, and
// reports whether it is placed (true when no recv is armed). It is the first
// step of cancelConnOps, on its own so hijackConn can make it the whole of
// its decision before it changes anything (celeris#869).
func (w *Worker) cancelRecvOp(fd int, cs *connState) bool {
	if !cs.recvArmed {
		return true
	}
	sqe := w.getCancelSQE()
	if sqe == nil {
		return false
	}
	prepCancelUserDataSkipSuccess(sqe, encodeUserDataGen(udRecv, fd, cs.generation))
	setSQEUserData(sqe, encodeUserData(udProvide, fd))
	return true
}

// cancelOtherOps is the rest of cancelConnOps: the cancels of an in-flight
// send and of the armed header timer. It reports whether a send's cancel was
// needed and not placed; the timer's is best effort, as it always was.
func (w *Worker) cancelOtherOps(fd int, cs *connState) (missed bool) {
	if cs.sending || cs.zcNotifPending {
		if sqe := w.getCancelSQE(); sqe != nil {
			prepCancelUserDataSkipSuccess(sqe, encodeUserDataGen(udSend, fd, cs.generation))
			setSQEUserData(sqe, encodeUserData(udProvide, fd))
		} else {
			missed = true
		}
	}
	// Cancel the armed slowloris header timer too. armHeaderTimer leaves an
	// IORING_OP_TIMEOUT in flight that fires ReadHeaderTimeout (default 10s)
	// after arm; handleHeaderTimer's "we don't cancel, only let it fire"
	// predated the close-path cancel infrastructure. Leaving it pending means
	// every closed connection — especially under churn-close (keep-alive off,
	// one request per conn) — accumulates a per-conn kernel timer that only
	// expires 10s later, pressuring the kernel timer-wheel and posting a
	// delayed -ETIME CQE storm that competes with live request CQEs. Cancel
	// it here so it terminates promptly as -ECANCELED instead (handleHeaderTimer
	// no-ops on a closing conn). This does NOT change slowloris defence: the
	// timer is still armed and enforced for live connections; it is only
	// reaped when the conn is already being torn down. Safe against the
	// release gate: udHeaderTimer is not a terminalOp (staleConnCQE) and the
	// timer is outside kernelInflight accounting, so its -ECANCELED CQE can
	// never decrement the recv/send in-flight count or release cs early.
	if cs.headerTimerArmed {
		if sqe := w.getCancelSQE(); sqe != nil {
			prepCancelUserDataSkipSuccess(sqe, encodeUserDataGen(udHeaderTimer, fd, cs.generation))
			setSQEUserData(sqe, encodeUserData(udProvide, fd))
		}
	}
	return missed
}

// retryMissedCancel places again the cancels a close path could not place for
// cs (cs.cancelMissed, celeris#869). The ops it aims them at may have ended
// meanwhile; a cancel of an op that is gone fails quietly (-ENOENT, dropped as
// every close-path cancel's failure is). Reports whether they were placed now.
// Worker thread only.
func (w *Worker) retryMissedCancel(cs *connState) bool {
	if w.cancelConnOps(cs.fd, cs) {
		return false
	}
	cs.cancelMissed = false
	return true
}

// getCancelSQE returns an SQE for a close-path cancel, submitting the
// pending SQ ring once to make room if it is full (the armHeaderTimer
// pattern). Returns nil only if the ring is full even after the submit.
func (w *Worker) getCancelSQE() unsafe.Pointer {
	if w.cancelSQEFull != nil && w.cancelSQEFull() {
		return nil
	}
	sqe := w.ring.GetSQE()
	if sqe != nil {
		return sqe
	}
	if _, err := w.ring.Submit(); err != nil {
		return nil
	}
	return w.ring.GetSQE()
}

// noteClosedInflight registers cs in Worker.closedOps when it still has
// kernel-held ops at close time, so their terminal CQEs — which arrive
// after w.conns[fd] is niled and therefore dispatch as stale — can be
// attributed back to cs (noteStaleTerminalOp) and unblock its release.
// Must be called by every close path that queues cs for deferred release,
// after the last arm/cancel decision for cs has been made.
func (w *Worker) noteClosedInflight(cs *connState) {
	if cs.kernelInflight <= 0 {
		return
	}
	if w.closedOps == nil {
		w.closedOps = make(map[uint64]*closedOpsEntry)
	}
	key := encodeConnOpKey(cs.fd, cs.generation)
	e := w.closedOps[key]
	if e == nil {
		e = &closedOpsEntry{}
		w.closedOps[key] = e
	}
	e.inflight += cs.kernelInflight
	e.fdOps += int16(fdOps(cs))
	if zcSendOwed(cs) {
		e.zcOwed++
	}
	e.conns = append(e.conns, cs)
}

// dropClosedOps removes cs's identity from Worker.closedOps when the
// wall-clock backstop releases it with ops still unaccounted for. Without
// this, a terminal CQE arriving after the backstop would write through
// the map into a connState the pool may have already handed to a new
// conn. Any conns colliding on the same identity lose their accounting
// too and will be reaped by their own backstop — acceptable for a path
// that only fires on kernel anomalies.
//
// It is also where the accounting lets go of a SEND_ZC the kernel may still
// read cs.sendBuf for (celeris#812): the backstop holds an entry that owes
// one (closedZCOwed), so an identity dropped with zcOwed above zero is a send
// buffer given up while the kernel may still send from it. Counted
// (CloseZCNotifForced); must stay 0.
func (w *Worker) dropClosedOps(cs *connState) {
	if len(w.closedOps) == 0 {
		return
	}
	key := encodeConnOpKey(cs.fd, cs.generation)
	if e := w.closedOps[key]; e != nil && e.zcOwed > 0 {
		w.handoffLoss.noteCloseZCNotifForced()
	}
	delete(w.closedOps, key)
}

// queuePendingRelease enqueues cs for deferred release: drainPendingRelease
// recycles it once cs.kernelInflight hits zero (or the wall-clock backstop
// fires). See Worker.pendingRelease docstring for the kernel-buffer-lifetime
// invariant this enforces.
func (w *Worker) queuePendingRelease(cs *connState) {
	w.queuePendingReleaseFD(cs, false, -1)
}

// queuePendingReleaseFD is queuePendingRelease (detached false) or
// queuePendingReleaseDetached (detached true) for a close path that may also
// hand the descriptor over: keptFD >= 0 is closed by drainPendingRelease when
// cs is released, not by the caller (celeris#685, closeFDOwed); -1 means the
// caller closes it. Worker thread only.
func (w *Worker) queuePendingReleaseFD(cs *connState, detached bool, keptFD int) {
	w.pendingRelease = append(w.pendingRelease, pendingReleaseEntry{
		cs:             cs,
		releaseAtNanos: time.Now().UnixNano() + pendingReleaseHoldNanos,
		detached:       detached,
		holdsFD:        keptFD >= 0,
		fd:             int32(keptFD),
	})
	if keptFD >= 0 {
		w.closeFDOwed++
		w.closeFDDeferredBatch++
	}
}

// queuePendingReleaseDetached holds cs alive past the kernel's recv-SQE
// drain window without recycling it through sync.Pool. Used by the
// detached close path (async-dispatch HTTP/1.1, WebSocket, SSE), where
// goroutine closures still reference cs.h1State / cs.asyncCond /
// cs.asyncInBuf — the worker observed asyncClosed and tore the conn
// down, but the dispatch goroutine can still be inside its deferred
// recover() block, reading cs.fd and re-clearing cs.asyncInBuf, when
// finishCloseDetached returns. Recycling cs through releaseConnState
// at this point races those reads. We just keep the strong ref alive
// until the kernel ops drain (cs.kernelInflight == 0) and let GC
// collect — the goroutine's own closure references keep cs alive for
// however long it still needs it, but only THIS queue's strong ref is
// visible on behalf of the kernel's invisible recv pointer, so it must
// outlive every pending op targeting cs.buf // span-corruption class).
func (w *Worker) queuePendingReleaseDetached(cs *connState) {
	w.queuePendingReleaseFD(cs, true, -1)
}

// drainPendingRelease releases queued connStates whose kernel-held ops
// have all delivered their terminal CQE (cs.kernelInflight == 0 — the
// close path's ASYNC_CANCELs make that prompt), compacting the queue in
// place. Entries still holding kernel ops stay queued until their
// wall-clock backstop (releaseAtNanos vs cachedNow; the deadline is a
// fresh time.Now()+hold from enqueue so the window is real time even
// when cachedNow lags — v1.5.0 review 2.9). The backstop firing means
// the kernel never delivered a terminal CQE for an op we believe it
// holds — log a WARN, since releasing now trades a potential
// use-after-free against an unbounded leak, and scrub the conn from
// closedOps so a later CQE cannot touch the released memory. The one
// exception is an entry that still owes a SEND_ZC (celeris#812,
// closedZCOwed): its send buffer is held past the backstop until the
// notification arrives, off this walk (Worker.zcHolds), see
// pendingReleaseHoldNanos.
//
// Detached entries skip the pool recycle (releaseConnState would
// reset fields that goroutine closures may still observe via the
// runAsyncHandler defer block) and just drop the strong ref so GC
// can reclaim the cs.
//
// In steady state entries drain within a loop pass or two, so the queue
// stays a handful of entries deep and the full scan is cheap; it is the
// straggler entries themselves that would otherwise block a FIFO-prefix
// scan, so compaction is required for prompt release behind them.
func (w *Worker) drainPendingRelease() {
	kept := w.pendingRelease[:0]
	// One cancel retry per pass, for the first entry that needs it: a SQ
	// ring that stays full would otherwise cost a failing submit per entry
	// per pass (celeris#869).
	retried := false
	for i := range w.pendingRelease {
		entry := &w.pendingRelease[i]
		cs := entry.cs
		if cs.kernelInflight > 0 {
			// The close could not place the cancel of an op it left armed
			// (cancelConnOps): place it now. The backstop's window restarts
			// at the placement, for the cancel's terminal completion to arrive.
			if cs.cancelMissed && !retried {
				retried = true
				if w.retryMissedCancel(cs) {
					entry.releaseAtNanos = time.Now().UnixNano() + pendingReleaseHoldNanos
				}
			}
			// A kept descriptor goes as soon as no owed op names it
			// (celeris#798): what is left may be SEND_ZC notifications only,
			// and one of those can wait on a stalled peer for as long as the
			// socket is open. cs itself stays until they arrive: they say the
			// kernel is done with its send buffer.
			if entry.holdsFD && !w.closedFDNamed(cs) {
				w.releaseKeptFD(entry)
			}
			if entry.releaseAtNanos > w.cachedNow {
				kept = append(kept, *entry)
				continue
			}
			// Past the backstop. A SEND_ZC the kernel may still read
			// cs.sendBuf for is no anomaly (celeris#812): the send buffer is
			// held, off this walk, until that op's notification, and the
			// backstop gives up only the descriptor, as it always has
			// (holdZCPastBackstop).
			//
			// Not while an op whose cancel was never placed still names the
			// descriptor (below): holdZCPastBackstop gives the descriptor up
			// and takes the entry off this walk, which ends the cancel's
			// retries. Only a SEND_ZC notification left (closedFDNamed false)
			// is the celeris#812 case.
			missedAndNamed := cs.cancelMissed && w.closedFDNamed(cs)
			if !missedAndNamed && w.closedZCOwed(cs) {
				w.holdZCPastBackstop(entry)
				continue
			}
			// An op nobody asked the kernel to end is no anomaly either
			// (celeris#869): the cancel was never placed, so the recv or
			// send may still be armed and write (or read) the buffers
			// below. Held in place, the cancel retried on each pass, until
			// its terminal CQE: a leak only as long as the SQ ring stays
			// full and the kernel leaves the op running after the close's
			// shutdown.
			if cs.cancelMissed {
				if !entry.cancelHeld {
					entry.cancelHeld = true
					w.handoffLoss.noteCloseCancelMissedHeld()
					if w.logger != nil {
						w.logger.Warn("holding a closed connection's buffers past the release backstop: the cancel of an op it still owes was never placed",
							"worker", w.id, "fd", cs.fd, "generation", cs.generation,
							"inflight", cs.kernelInflight, "detached", entry.detached,
							"holds_fd", entry.holdsFD)
					}
				}
				kept = append(kept, *entry)
				continue
			}
			// Backstop: kernel anomaly, not normal flow.
			if w.logger != nil {
				w.logger.Warn("releasing connState with kernel ops unaccounted for after backstop hold",
					"worker", w.id, "fd", cs.fd, "generation", cs.generation,
					"inflight", cs.kernelInflight, "detached", entry.detached,
					"holds_fd", entry.holdsFD)
			}
			w.dropClosedOps(cs)
			// The descriptor goes too, though an op may still name it: a
			// descriptor held forever is a leak with no end. The close path
			// shut the socket's read side down, so an owed recv the kernel
			// issues on THIS socket ends at once; one issued after the
			// number is reused would not, which is why this is counted and
			// must stay 0 (celeris#685).
			if entry.holdsFD {
				w.handoffLoss.noteCloseFDForced()
			}
		}
		// The close the close path left for this moment (celeris#685):
		// every op that named the descriptor has delivered its terminal
		// CQE, so none can resolve the number any more.
		if entry.holdsFD {
			w.releaseKeptFD(entry)
		}
		if !entry.detached {
			releaseConnState(cs)
		}
	}
	// Nil out the tail slots so the backing array drops its strong refs
	// to released connStates.
	for i := len(kept); i < len(w.pendingRelease); i++ {
		w.pendingRelease[i].cs = nil
	}
	w.pendingRelease = kept
}

func (w *Worker) finishClose(fd int) {
	cs := w.conns[fd]
	// Unlink from the dirty list before the connState leaves w.conns
	// (celeris#527). Nothing downstream can do it: the dirty loop's only
	// removeDirty sits inside "if !cs.sending", so a conn torn down with a
	// SEND in flight is skipped by the very code that would unlink it, and
	// releaseConnState clears cs.dirty before pooling the object, which
	// turns a later removeDirty into a no-op while the predecessor's
	// dirtyNext still points at it — truncating the list and stranding
	// every entry behind it. removeDirty is idempotent, so this is purely
	// additive next to closeConn's existing unlink.
	if cs != nil {
		w.removeDirty(cs)
	}
	// Remove from liveConns BEFORE niling w.conns[fd]: removeLiveConn swaps
	// the last live entry into cs.liveIdx and updates that swapped-in
	// connState's liveIdx via w.conns[swappedFD], so the conns slice must
	// still be intact (v1.5.0 review 1.8 hazard).
	w.removeLiveConn(cs)
	w.conns[fd] = nil
	w.connCount--
	w.activeConns.Add(-1)
	w.closeCount.Add(1)

	if w.cfg.OnDisconnect != nil && cs != nil {
		w.cfg.OnDisconnect(cs.remoteAddr)
	}
	// A nil connState here means the gauge and closeCount moved above but
	// no OnDisconnect could fire: the close is invisible to every
	// hook-derived counter. The nil guard was written because the author
	// judged the state reachable, so count the reach instead of leaving
	// the path silent (celeris#624). Must stay 0.
	if cs == nil && w.closeMissingConnState != nil {
		w.closeMissingConnState.Add(1)
	}

	// Capture close-path decisions before queueing cs for deferred release.
	fixedFile := cs != nil && cs.fixedFile
	fastClose := cs != nil && engine.Protocol(cs.protocol.Load()) == engine.HTTP1 && cs.h1State != nil && !cs.h1State.Detached.Load()
	// The fd-lifetime rule (celeris#685): while the kernel still owes an op
	// that names fd, the number is not released here. See fdOwed.
	owed := fdOwed(cs)
	// Cancel-then-release discipline (v1.4.15/7beebb9 corruption fix): ASYNC_CANCEL any kernel-held
	// op still targeting cs's buffers (the single-shot recv is virtually
	// ALWAYS armed here — closing the fd below does NOT complete it), then
	// register cs for stale-CQE accounting and defer the pool release
	// until every op has delivered its terminal CQE. Returning cs to
	// sync.Pool before that would let Go's GC reclaim cs.buf's backing
	// array; the kernel's still-pending recv SQE would then write
	// straggler bytes into memory Go has repurposed — the #256 stackalloc
	// SIGSEGV / Green-Tea-GC span-corruption class.
	if cs != nil {
		// celeris#715 witness, hold and control, validation builds only:
		// see recvUnsubmitted and recvLinkedOwed.
		if recvtheft.Enabled {
			if hold, linked := w.recvTheftWitness(cs); hold {
				defer recvtheft.HoldAfterClose(w.id, fd, linked)
			}
		}
		cs.cancelMissed = w.cancelConnOps(fd, cs)
		w.noteClosedInflight(cs)
		w.queuePendingReleaseFD(cs, false, keptFD(fd, owed))
		if recvtheft.Enabled && recvtheft.SubmitBeforeClose() {
			_, _ = w.ring.Submit()
		}
	}

	if fixedFile {
		// Fixed file: close via io_uring direct close (no real FD to shutdown).
		// Stamp the closing conn's generation so its close CQE is matched to
		// THIS occupant — a stale close-error CQE for a freed+reused slot is
		// dropped at dispatch before reaching handleClose, so it can no longer
		// nil the new occupant (review 2.2 error path). cs is still valid here:
		// queuePendingRelease only DEFERS releaseConnState.
		sqe := w.ring.GetSQE()
		if sqe != nil {
			prepCloseDirect(sqe, fd)
			gen := uint32(0)
			if cs != nil {
				gen = cs.generation
			}
			setSQEUserData(sqe, encodeUserDataGen(udClose, fd, gen))
		}
		// Explicitly reset the fixed file slot to -1 so the kernel's
		// IORING_FILE_INDEX_ALLOC allocator can reuse it. Without this,
		// some kernels (e.g., AWS 6.17) fail to recycle CLOSE_DIRECT'd
		// slots, exhausting the 65536-entry table under sustained churn.
		_ = w.ring.UpdateFixedFile(fd, -1)
		return
	}

	// Fast-close path for H1 non-detached connections: close() alone.
	// The response bytes we wrote are already in the kernel send buffer
	// and will go out before the socket tears down; localhost ACKs
	// within microseconds. Skipping shutdown(SHUT_WR) + the recv drain
	// saves two syscalls per close and is the difference between hertz
	// territory (~30 k rps) and ~24 k rps under bench-harness churn.
	//
	// The graceful path (shutdown + drain + close) is retained for H2
	// because GOAWAY / RST_STREAM frames can be staged in the send
	// buffer at close time; shutdown is what pushes FIN after those
	// frames so the peer sees them. For H1 without a body in-flight,
	// close() without shutdown is equivalent on localhost and within
	// noise on real networks.
	//
	// forceRSTClose (slowloris-defence): SHUT_RDWR + close. See
	// finishCloseDetached for the rationale.
	//
	// Each branch below ends in closeUnlessOwed: with an op still owed the
	// descriptor stays open, and drainPendingRelease closes it at that op's
	// terminal CQE (celeris#685). A branch that did not shut the read side
	// down already does so first, so the owed recv ends as soon as the
	// kernel issues it (fdOwed).
	if cs != nil && cs.forceRSTClose {
		_ = unix.Shutdown(fd, unix.SHUT_RDWR)
		closeUnlessOwed(fd, owed)
		return
	}
	// fastClose: plain H1 no-body conn → close() alone suffices.
	if fastClose {
		if owed {
			_ = unix.Shutdown(fd, fastCloseShutdownHow(cs))
		}
		closeUnlessOwed(fd, owed)
		return
	}
	_ = unix.Shutdown(fd, shutdownHow(owed))
	raddr := ""
	if cs != nil {
		raddr = cs.remoteAddr
	}
	sockopts.CloseDrain(fd, "iouring/finishClose", raddr)
	closeUnlessOwed(fd, owed)
}

// finishCloseAny dispatches to finishCloseDetached for detached connections
// and finishClose otherwise. Used by the deferred-close paths in
// handleSend / completeSend, where the connection may be either a plain
// H1/H2 conn or a detached WS/SSE conn (distinguished by detachMu).
func (w *Worker) finishCloseAny(fd int, cs *connState) {
	if cs.detachMu != nil {
		w.finishCloseDetached(fd, cs)
		return
	}
	w.finishClose(fd)
}

// finishCloseDetached closes the FD and removes the connection from bookkeeping
// WITHOUT returning the connState to the pool. Used when a detached goroutine
// still holds closure references to the connState.
func (w *Worker) finishCloseDetached(fd int, cs *connState) {
	// Unlink from the dirty list first (celeris#527). Detached entries skip
	// releaseConnState entirely, so one torn down with cs.sending true is
	// otherwise immortal: nothing ever clears sending (the cancelled SEND's
	// -ECANCELED is dropped by staleConnCQE once w.conns[fd] is nil), the
	// dirty loop skips it forever, and adaptiveTimeout returns 0 while
	// dirtyHead is non-nil — so the worker busy-spins at 100% CPU even idle.
	w.removeDirty(cs)
	// Remove from liveConns BEFORE niling w.conns[fd] (same hazard as
	// finishClose — removeLiveConn touches w.conns[swappedFD]).
	w.removeLiveConn(cs)
	w.conns[fd] = nil
	w.connCount--
	w.activeConns.Add(-1)
	w.closeCount.Add(1)

	if w.cfg.OnDisconnect != nil {
		w.cfg.OnDisconnect(cs.remoteAddr)
	}

	fixedFile := cs.fixedFile
	// The fd-lifetime rule (celeris#685), as in finishClose: see fdOwed.
	owed := fdOwed(cs)
	// Do NOT call releaseConnState — goroutine closures still reference cs.
	//
	// Cancel-then-release discipline (v1.4.15/7beebb9 corruption fix): ASYNC_CANCEL the armed recv
	// (closing the fd below does NOT complete it — the op holds its own
	// file reference), then hold cs alive in pendingRelease until its
	// terminal CQE arrives. After the goroutine exits (which happens
	// promptly once closeConn sets asyncClosed and broadcasts asyncCond),
	// only this queue's strong ref stands in for the kernel's invisible
	// recv pointer. Without it, GC would reclaim cs.buf while the kernel
	// still has a pending recv SQE targeting &cs.buf[0]; a straggler
	// segment then writes HTTP bytes into memory Go has repurposed —
	// historically a stackalloc SIGSEGV at addr 0x48202f20544547 (#256,
	// strict-matrix churn-close × iouring-async), and on Go 1.26 a fatal
	// "s.allocCount != s.nelems" span corruption (v1.4.15/7beebb9: the old 100 ms
	// wall-clock hold sat below TCP's 200 ms RTO_MIN, so retransmitted
	// POST segments landed after release).
	// celeris#715 witness, hold and control, validation builds only: see
	// recvUnsubmitted and recvLinkedOwed.
	if recvtheft.Enabled {
		if hold, linked := w.recvTheftWitness(cs); hold {
			defer recvtheft.HoldAfterClose(w.id, fd, linked)
		}
	}
	cs.cancelMissed = w.cancelConnOps(fd, cs)
	w.noteClosedInflight(cs)
	w.queuePendingReleaseFD(cs, true, keptFD(fd, owed))
	if recvtheft.Enabled && recvtheft.SubmitBeforeClose() {
		_, _ = w.ring.Submit()
	}

	if fixedFile {
		sqe := w.ring.GetSQE()
		if sqe != nil {
			prepCloseDirect(sqe, fd)
			// Stamp the closing conn's generation (review 2.2 error path) —
			// cs is the non-nil param and stays valid (deferred release).
			setSQEUserData(sqe, encodeUserDataGen(udClose, fd, cs.generation))
		}
		_ = w.ring.UpdateFixedFile(fd, -1)
		return
	}

	// forceRSTClose: slowloris-defence path requested RST. Empirical
	// data (nightly 26418830572 vs 26414322152): SHUT_RDWR + close
	// gives ~50% more reliable observable-RST than plain close +
	// LINGER on iouring. The SHUT_RDWR + LINGER combo wins because
	// SHUT_RDWR drops the receive queue too (close() with LINGER
	// only drops TX). Walker observes the abortive close immediately.
	//
	// Each branch ends in closeUnlessOwed, and shuts the read side down too
	// when an op is still owed (shutdownHow), exactly as finishClose does.
	if cs.forceRSTClose {
		_ = unix.Shutdown(fd, unix.SHUT_RDWR)
		closeUnlessOwed(fd, owed)
		return
	}
	// Async-mode HTTP1 conns are NOT truly detached (no WS/SSE middleware
	// owns them) — they just pre-allocate detachMu in acquireConnState so
	// the dispatch goroutine and worker can serialize writeBuf access.
	//
	// Plain unix.Close (matching net/http) sends FIN if the kernel recv
	// buffer is empty, RST if non-empty. For slowloris, walker is still
	// writing drips so the buffer often has unread bytes — close → RST.
	// RST is NOT retransmitted by TCP, so if it's lost on the cluster
	// fabric the walker never observes the close (walker hangs).
	//
	// SHUT_WR forces FIN explicitly (writer half-close) BEFORE close,
	// regardless of recv-buffer state. FIN IS retransmitted from
	// FIN_WAIT_1 until ACK'd, so packet loss can't strand the walker.
	// No recv drain (which was the prior approach's race source —
	// the drain syscall and the close syscall left a multi-µs window in
	// which a fresh walker drip would queue, making the close → RST).
	if cs.h1State != nil && !cs.h1State.Detached.Load() {
		_ = unix.Shutdown(fd, shutdownHow(owed))
		closeUnlessOwed(fd, owed)
		return
	}
	// Truly detached (WS/SSE): graceful half-close so middleware-queued
	// close-frame echoes flush before the FIN. Sync unix.Close (not via
	// io_uring) to avoid the async-SQE pile-up that plagued the pre-patch
	// version.
	_ = unix.Shutdown(fd, shutdownHow(owed))
	sockopts.CloseDrain(fd, "iouring/finishCloseDetached", cs.remoteAddr)
	closeUnlessOwed(fd, owed)
}

// promoteConnToAsync hands a conn that bailed from the inline fast path
// (ErrAsyncDispatch) to its per-conn dispatch goroutine, seeding it with the
// stashed request bytes. Mirrors the tail of the async-dispatch block. The
// caller has already set cs.asyncPromoted and returned the provided buffer.
func (w *Worker) promoteConnToAsync(cs *connState, _ int, stashed []byte, c *completionEntry) {
	// The header deadline is read before the dispatch goroutine starts: it
	// runs the stashed request at once, and an h2c upgrade sets cs.h1State to
	// nil there (celeris#722). Armed below, from the value.
	hdrDL := w.asyncHeaderDeadline(cs)
	cs.asyncInMu.Lock()
	cs.asyncInBuf = append(cs.asyncInBuf, stashed...)
	starting := !cs.asyncRun
	if starting {
		cs.asyncRun = true
	}
	cs.asyncInMu.Unlock()
	// celeris#715 hypothesis (c), validation builds only: widen the window
	// between the unlock and the goroutine's wake-up (recvtheft.SetWakeHold).
	if recvtheft.Enabled {
		recvtheft.WakeHold()
	}
	if starting {
		w.asyncWG.Add(1)
		go w.runAsyncHandler(cs)
	} else {
		cs.asyncCond.Signal()
	}
	w.reqBatch++
	if hdrDL > 0 {
		w.armHeaderTimerAt(cs, hdrDL)
	}
	if !cqeHasMore(c.Flags) && !cs.recvPaused {
		if !w.prepareRecv(cs, cs.buf) {
			cs.needsRecv = true
			w.markDirty(cs)
		}
	}
	// celeris#715 hypothesis (a), validation builds only: with a trial armed,
	// let the dispatch goroutine queue its close before this iteration
	// reaches drainDetachQueue, so the recv just re-armed is still
	// unsubmitted when closeConn runs (recvtheft.Options.PromoteGate).
	if recvtheft.Enabled {
		recvtheft.AfterPromoteArm(func() bool { return w.detachQPending.Load() != 0 })
	}
}

// asyncHeaderDeadline returns the header deadline the worker should arm a
// kernel timer for on a promoted async conn, or 0 for none: none configured,
// a timer already in flight, no H1 state, or the conn's detachMu held.
//
// The worker may not read cs.h1State while the conn's dispatch goroutine can
// be running: that goroutine owns the H1 state across ProcessH1 and sets
// cs.h1State to nil when the request is an h2c upgrade (switchToH2Local), so
// an unlocked read races that write and, between its nil check and its
// dereference, can take a nil pointer (celeris#722). The callers read before
// they start or feed the goroutine, and the read goes through
// snapshotH1Deadlines, under detachMu, the lock switchToH2Local runs under,
// and with TryLock, never Lock (celeris#593): a held lock means the goroutine
// is inside a request, which is not a moment a header deadline is waiting on,
// and the arm is retried at the next feed. The checkTimeouts sweep is the
// fallback for a timer that is not armed, as it is for one the SQ ring
// dropped. Worker thread.
func (w *Worker) asyncHeaderDeadline(cs *connState) int64 {
	if w.cfg.ReadHeaderTimeout <= 0 || cs.headerTimerArmed {
		return 0
	}
	snap, ok := snapshotH1Deadlines(cs)
	if !ok || !snap.haveH1 {
		return 0
	}
	return snap.hdrDL
}

// canRevertToInline reports whether a promoted conn should be reverted to the
// inline fast path (celeris#364). True when single-shot recv is in use, the
// conn recorded the route that promoted it, and that route's promotion has
// since expired (RouteAsync now false — the route-level TTL de-promotion).
// Called by serveAsync ONLY while holding asyncInMu with asyncInBuf empty.
func (w *Worker) canRevertToInline(cs *connState) bool {
	return w.bufRing == nil && cs.promotedPath != "" && cs.h1State != nil &&
		cs.h1State.RouteAsync != nil &&
		// Clean request boundary only: never revert mid-request (a partial body
		// or buffered headers still accumulating), so h1State ownership flips
		// back to the worker between requests, exactly like a fresh inline conn.
		!cs.h1State.HasPendingData() &&
		!cs.h1State.RouteAsync(cs.promotedMethod, cs.promotedPath)
}

// runAsyncHandler is cs's dispatch goroutine: serveAsync, plus the teardown
// for a serveAsync that does not return (celeris#791).
//
// celeris.Server's router recovers a handler panic itself, on this path as
// on the inline one, and answers 500. What reaches here is a panic that
// escapes the router (a stream.Handler used directly, or the engine's own
// request handling) and a runtime.Goexit (t.FailNow in a test handler, for
// one), which no recover stops. Either can unwind serveAsync from inside
// ProcessH1, where this goroutine holds cs.detachMu; abortAsyncHandler
// releases it and gives the conn the teardown a handler error gets.
func (w *Worker) runAsyncHandler(cs *connState) {
	defer w.asyncWG.Done()
	// held: this goroutine holds cs.detachMu (serveAsync sets it after each
	// Lock and clears it before each Unlock). returned: serveAsync returned,
	// so neither a panic nor a Goexit is unwinding through here.
	var held, returned bool
	defer func() {
		if !returned {
			w.abortAsyncHandler(cs, recover(), held)
		}
	}()
	w.serveAsync(cs, &held)
	returned = true
}

// abortAsyncHandler tears cs down after its dispatch goroutine's handler
// panicked (r is the recovered value) or called runtime.Goexit (r is nil, and
// the goroutine ends when this returns). Deferred by runAsyncHandler, on the
// dispatch goroutine.
//
// It releases cs.detachMu first, if this goroutine still holds it: held, and
// not already released on this goroutine's behalf by a Detach inside
// ProcessH1 (asyncDetachUnlocked, celeris#273; Unlocking that again would be
// a fatal "unlock of unlocked mutex", cf. celeris#309). That is before the
// log, too, whose handler is the application's and may be slow, while the
// worker's shutdown and the guarded writes of a detached conn take the lock
// unconditionally. The rest is the handler-error teardown in serveAsync:
// asyncClosed, endDispatch under asyncInMu, then the hand-back through the
// detach queue, whose asyncClosed branch runs closeConn on the worker. Like
// that path it never holds two of detachMu, asyncInMu and detachQMu at once.
// Nothing here touches cs after the enqueue.
//
// The lock is free from the release on, so a closeConn that meets cs before
// the hand-back (a recv FIN or error, the timeout reap, shutdown) takes it
// and closes the conn while this goroutine still logs: the fd is closed and
// may be reissued, and CloseH1 releases the stream (celeris#844 tracks a
// panic value that shares its memory). cs stays valid: a conn that has a
// detachMu is closed by finishCloseDetached, which never pools it
// (queuePendingReleaseDetached), and the drain skips the hand-back of a conn
// already detachClosed. endDispatch clears asyncRun after the release, so a
// worker that finds the lock held while the goroutine reads as gone
// (dispatchBusy false) waits only for a bounded holder.
func (w *Worker) abortAsyncHandler(cs *connState, r any, held bool) {
	if held && !cs.asyncDetachUnlocked {
		cs.detachMu.Unlock()
	}
	if w.logger != nil {
		if r != nil {
			w.logger.Error("async handler panicked",
				"panic", r,
				"stack", string(debug.Stack()),
				"fd", cs.fd,
			)
		} else {
			w.logger.Error("async handler exited without returning (runtime.Goexit)",
				"stack", string(debug.Stack()),
				"fd", cs.fd,
			)
		}
	}
	cs.asyncClosed.Store(true)
	cs.asyncInMu.Lock()
	cs.asyncInBuf = cs.asyncInBuf[:0]
	cs.endDispatch() // enqueued below: that is the hand-back
	cs.asyncInMu.Unlock()
	// Wake the worker so it observes asyncClosed and tears
	// down the conn via the detachQueue → drain path.
	w.enqueueDetach(cs)
}

// serveAsync is the dispatch goroutine's loop (see runAsyncHandler). It
// drains cs.asyncInBuf: it takes the currently-buffered bytes, runs
// ProcessH1 under cs.detachMu (serializing with the worker's writeBuf and
// sendBuf mutations), and hands what it could not write to the worker
// through the detach queue, so the worker submits the SEND SQEs on its own
// goroutine (SINGLE_ISSUER: a handler goroutine cannot call ring.GetSQE).
// It preserves HTTP/1.1 pipelining: ProcessH1's offset loop drains every
// request in the slice in order, and the responses land on cs.writeBuf in
// the same order before the flush.
//
// *held tracks its Lock and Unlock of cs.detachMu, for abortAsyncHandler.
// It stays set across a Detach inside ProcessH1, which releases the lock on
// this goroutine's behalf and records that in asyncDetachUnlocked instead.
func (w *Worker) serveAsync(cs *connState, held *bool) {
	for {
		cs.asyncInMu.Lock()
		if cs.relinkOwed {
			// The worker gave this conn up while our handler held detachMu
			// (celeris#704: the dirty-list pass); hand it back now that the
			// handler's writes are flushed as far as they go, so the worker
			// re-examines it. asyncInMu -> detachQMu: nothing takes
			// asyncInMu under detachQMu.
			cs.relinkOwed = false
			w.enqueueDetach(cs)
		}
		cs.asyncParked = true
		for len(cs.asyncInBuf) == 0 && !cs.asyncClosed.Load() {
			// celeris#364: revert this conn to inline when the route that
			// promoted it has de-promoted (its TTL expired). Safe ONLY here:
			// asyncInBuf is empty (no in-flight input, last response already
			// written) and we hold asyncInMu, which the worker's feed path
			// re-acquires and re-checks asyncPromoted against — so clearing it
			// here cannot race a concurrent feed. The worker owns recv and
			// resumes the inline fast path on the next CQE.
			if w.canRevertToInline(cs) {
				cs.asyncPromoted.Store(false)
				// Nothing is owed here in practice: a debt is only taken on
				// while the goroutine runs outside this loop, and the loop
				// top above hands a relink back. Checked all the same.
				owed := cs.endDispatch()
				cs.asyncInMu.Unlock()
				if owed {
					w.enqueueDetach(cs)
				}
				return
			}
			// #383 reverse (async): at a clean park boundary while an
			// io_uring→epoll drain is active, hand THIS conn to epoll. We are at
			// a true boundary (asyncInBuf empty, last response direct-written and
			// flushed). Mark + enqueue + EXIT — the worker finishes the SQE work
			// (cancel recv) + dup + hand-off in drainDetachQueue, after we are
			// gone, so there is no goroutine-vs-release race. If a new request
			// arrives first, the feed path clears transplantPending and respawns
			// us, so no request is lost. SINGLE_ISSUER: we submit no SQE here.
			// On a worker that cannot reap, no conn is eligible: the claim
			// could only be refused, so we park instead (celeris#681 R1).
			if w.transplant.Load() != nil && w.asyncTransplantEligible(cs) {
				cs.transplantPending.Store(true)
				cs.endDispatch() // enqueued below: that is the hand-back
				cs.asyncInMu.Unlock()
				w.enqueueDetach(cs)
				return
			}
			cs.asyncCond.Wait()
		}
		cs.asyncParked = false
		if cs.asyncClosed.Load() {
			// The one loop exit that does not otherwise enqueue cs: a close
			// requested while this goroutine ran a handler may have been
			// left to it (closeOwed, celeris#704), and then this is where it
			// is handed back.
			owed := cs.endDispatch()
			cs.asyncInMu.Unlock()
			if owed {
				w.enqueueDetach(cs)
			}
			return
		}
		cs.asyncInBuf, cs.asyncOutBuf = cs.asyncOutBuf[:0], cs.asyncInBuf
		data := cs.asyncOutBuf
		cs.asyncInMu.Unlock()

		// Post-detach iterations (WS / SSE): ProcessH1's only job is to
		// deliver `data` to state.WSDataDelivery (writes flow through the
		// guarded writeFn, which acquires cs.detachMu on its own). Taking
		// detachMu here would re-Lock it on every torture-frame delivery
		// — and the post-ProcessH1 branch below (asyncDetachUnlocked)
		// would then skip the symmetric Unlock, leaking the mutex.
		// Once leaked, the WS handler's writeCloseProtocol →
		// writeCloseFrame → guarded → cs.detachMu.Lock() deadlocks
		// forever, which is exactly the per-worker WS-handler hang that
		// celeris#284 surfaced (handler never returns, deferred ws.Close
		// never runs, idleDeadlineFn(1) never fires, conn stays
		// "detached forever," subsequent /ws upgrades on the same worker
		// accumulate stuck state). The pre-detach iteration (the one
		// where the WS middleware calls c.Detach inside ProcessH1) still
		// needs the lock because the handler chain may write a response
		// to cs.writeBuf before Detach fires; OnDetach itself drops the
		// lock on celeris#273's behalf (see line 974).
		acquiredDetachMu := false
		if !cs.asyncDetachUnlocked {
			cs.detachMu.Lock()
			acquiredDetachMu = true
			*held = true
		}
		// Re-check asyncClosed under detachMu (when we acquired it).
		// closeConn sets asyncClosed BEFORE taking detachMu to run
		// CloseH1; if we raced past the top-of-loop check but closeConn
		// acquired detachMu first, by the time we hold detachMu our
		// cs.h1State may already be torn down. Bail out here so we
		// don't call ProcessH1 on a closed state.
		if cs.asyncClosed.Load() {
			if acquiredDetachMu {
				*held = false
				cs.detachMu.Unlock()
			}
			// Nor does this one; see the loop-top exit.
			cs.asyncInMu.Lock()
			owed := cs.endDispatch()
			cs.asyncInMu.Unlock()
			if owed {
				w.enqueueDetach(cs)
			}
			return
		}
		processErr := conn.ProcessH1(cs.ctx, data, cs.h1State, w.handler, cs.writeFn)
		// H1→H2 upgrade on the async dispatch path. ProcessH1 has
		// written the 101 Switching Protocols response to cs.writeBuf
		// and stashed the upgrade info. Promote cs-local state now
		// (safe under detachMu), flush writeBuf synchronously, then
		// hand off to the worker to register the fd on the H2 write-
		// queue poll list. The goroutine exits — all subsequent recvs
		// dispatch via the inline H2 path (cs.protocol is now H2C).
		if errors.Is(processErr, conn.ErrUpgradeH2C) {
			promoteErr := w.switchToH2Local(cs)
			// NOT for a fixed-file conn: cs.fd is then a registered-file
			// TABLE INDEX, not a descriptor, so unix.Write would send the
			// 101 to whatever real fd holds that number (celeris#538).
			// Leaving the bytes in writeBuf takes the same disposition as
			// the EAGAIN branch below — the worker ring-sends them, and the
			// ring resolves the index correctly. Nor while a ring SEND of
			// the conn's earlier bytes is outstanding (celeris#751): the 101
			// would reach the client ahead of the rest of the previous
			// response. The worker sends writeBuf after that SEND completes.
			if promoteErr == nil && !cs.fixedFile && len(cs.writeBuf) > 0 && !ringSendOutstanding(cs) {
				n, werr := unix.Write(cs.fd, cs.writeBuf)
				switch {
				case werr == nil && n == len(cs.writeBuf):
					cs.writeBuf = cs.writeBuf[:0]
				case werr == nil:
					// Partial write — shift remainder; worker retries
					// via markDirty after we enqueue on detachQueue.
					remaining := len(cs.writeBuf) - n
					copy(cs.writeBuf, cs.writeBuf[n:])
					cs.writeBuf = cs.writeBuf[:remaining]
				case werr == unix.EAGAIN || werr == unix.EWOULDBLOCK:
					// Socket send buffer full — same disposition as a
					// partial write; bytes stay in cs.writeBuf.
				default:
					promoteErr = werr
				}
			}
			*held = false
			cs.detachMu.Unlock()
			if promoteErr != nil {
				// Fatal — route through the asyncClosed teardown so the
				// worker runs closeConn from its own goroutine.
				cs.asyncClosed.Store(true)
				cs.asyncInMu.Lock()
				cs.asyncInBuf = cs.asyncInBuf[:0]
				cs.endDispatch() // enqueued below: that is the hand-back
				cs.asyncInMu.Unlock()
			} else {
				cs.asyncInMu.Lock()
				cs.asyncInBuf = cs.asyncInBuf[:0]
				cs.endDispatch() // enqueued below: that is the hand-back
				cs.asyncInMu.Unlock()
				cs.asyncH2Promoted.Store(true)
			}
			w.detachQMu.Lock()
			w.detachQueue = append(w.detachQueue, cs)
			w.detachQPending.Store(1)
			w.detachQMu.Unlock()
			w.wakeFD.Signal()
			return
		}
		// celeris#273: a user handler may have called c.Detach() inside
		// ProcessH1 (websocket or sse middleware). OnDetach released
		// detachMu so subsequent guarded writeFn calls don't deadlock.
		// The dispatch goroutine no longer owns the lock — skip the
		// direct-write path (the bytes were already enqueued via
		// guarded → detachQueue/eventfd, the worker will flushSend
		// them) and skip the symmetric Unlock below.
		if cs.asyncDetachUnlocked {
			// ErrHijacked is a valid post-Detach return: the H1 parser
			// considers a hijacked conn "done with the request". Treat
			// it like nil here — the middleware now owns the conn and
			// is responsible for its lifetime via the done() callback.
			if processErr != nil && !errors.Is(processErr, conn.ErrHijacked) {
				cs.asyncClosed.Store(true)
				cs.asyncInMu.Lock()
				cs.asyncInBuf = cs.asyncInBuf[:0]
				cs.endDispatch() // enqueued below: that is the hand-back
				cs.asyncInMu.Unlock()
				w.detachQMu.Lock()
				w.detachQueue = append(w.detachQueue, cs)
				w.detachQPending.Store(1)
				w.detachQMu.Unlock()
				w.wakeFD.Signal()
				return
			}
			// Post-Detach: loop back to wait for more recv bytes (WS
			// frames delivered via WSDataDelivery, or SSE conn-close
			// detection). The handler runs in its own goroutine
			// spawned by the middleware; this dispatch goroutine just
			// shuttles RX bytes into ProcessH1 → WSDataDelivery.
			continue
		}
		// Direct-write fast path: on the async-handler goroutine, call
		// unix.Write(fd, writeBuf) inline instead of bouncing through
		// the detachQueue → eventfd → worker → SEND-SQE round-trip.
		// The iouring multishot recv on this fd is unaffected — TCP is
		// bidirectional and the kernel happily admits a concurrent write
		// from any goroutine even while a recv SQE is pending. Mirrors
		// the internal/engine/epoll runAsyncHandler shape (internal/engine/epoll/loop.go:990)
		// and closes the 3× integrated-Redis regression observed on
		// iouring (95 µs/op → target ~30 µs/op, matching epoll and
		// go-redis + stdlib).
		// A refused write (celeris#761) ends the conn as a request error
		// does: the worker's closeConn sends what was staged, then closes.
		if processErr == nil && cs.writeRefused {
			processErr = errWriteRefused
		}
		var partial bool
		if processErr == nil && cs.fixedFile && len(cs.writeBuf) > 0 {
			// Fixed-file conn: cs.fd is a registered-file TABLE INDEX, not a
			// descriptor, so the direct write below would go to whatever real
			// fd holds that number (celeris#538). Take the same route as a
			// full socket buffer — hand the bytes to the worker, whose ring
			// SEND resolves the index correctly.
			partial = true
		} else if processErr == nil && len(cs.writeBuf) > 0 && ringSendOutstanding(cs) {
			// A ring SEND of this conn's earlier bytes is outstanding: the
			// rest of a previous response whose own direct write was short
			// (celeris#751). Writing now would put these bytes on the wire
			// ahead of it, inside the previous response of a pipelining
			// client. Leave them in writeBuf, as for a full socket buffer:
			// the worker's completeSend sends writeBuf after that SEND.
			partial = true
		} else if processErr == nil && len(cs.writeBuf) > 0 {
			n, werr := unix.Write(cs.fd, cs.writeBuf)
			if werr != nil {
				if werr == unix.EAGAIN || werr == unix.EWOULDBLOCK {
					// Socket send buffer full — defer to the worker so
					// flushSend can retry under dirty-list management.
					partial = true
				} else {
					processErr = werr
				}
			} else if n < len(cs.writeBuf) {
				// Partial write — shift remainder and defer to worker.
				remaining := len(cs.writeBuf) - n
				copy(cs.writeBuf, cs.writeBuf[n:])
				cs.writeBuf = cs.writeBuf[:remaining]
				partial = true
			} else {
				cs.writeBuf = cs.writeBuf[:0]
			}
		}
		*held = false
		cs.detachMu.Unlock()

		if processErr != nil {
			cs.asyncClosed.Store(true)
			cs.asyncInMu.Lock()
			cs.asyncInBuf = cs.asyncInBuf[:0]
			cs.endDispatch() // enqueued below: that is the hand-back
			cs.asyncInMu.Unlock()
			// Wake the worker so it notices asyncClosed and runs closeConn
			// from its own goroutine via the detachQueue → drain path.
			w.detachQMu.Lock()
			w.detachQueue = append(w.detachQueue, cs)
			w.detachQPending.Store(1)
			w.detachQMu.Unlock()
			w.wakeFD.Signal()
			return
		}

		if partial {
			w.detachQMu.Lock()
			w.detachQueue = append(w.detachQueue, cs)
			w.detachQPending.Store(1)
			w.detachQMu.Unlock()
			w.wakeFD.Signal()
		}
	}
}

// ringSendOutstanding reports whether a ring SEND of cs's earlier bytes is in
// flight, or owed, so that a raw unix.Write of writeBuf now would reach the
// wire ahead of them (celeris#751): a SEND or its SEND_ZC notification is
// outstanding, or sendBuf/bodyBuf hold bytes the worker has taken from
// writeBuf and not yet sent. The worker mutates all four under cs.detachMu,
// which the caller holds, so no SEND can start while it does. The detached
// conns' guarded writeFn tests the same condition.
func ringSendOutstanding(cs *connState) bool {
	return cs.sending || cs.zcNotifPending || len(cs.sendBuf) > 0 || len(cs.bodyBuf) > 0
}

func (w *Worker) makeWriteFn(cs *connState) func([]byte) {
	return func(data []byte) {
		if cs.closing {
			return
		}
		// Back-pressure (an HTTP/2 or detached conn; see sendCap): refuse
		// the write only when the backlog before it is over the cap, and
		// say so: the site that ran the handler then closes the conn
		// (celeris#761), where it used to be left waiting for the refused
		// bytes.
		if len(cs.writeBuf)+len(cs.sendBuf)+len(cs.bodyBuf) > cs.sendCap() {
			cs.writeRefused = true
			return
		}
		// Append to writeBuf — no per-write allocation. The kernel holds
		// sendBuf (not writeBuf), so appending here is safe.
		// Don't markDirty here — handleRecv calls flushSend after the
		// handler returns. Only markDirty if flushSend fails (SQ ring
		// full), avoiding linked-list overhead on the happy path.
		cs.writeBuf = append(cs.writeBuf, data...)
	}
}

// prepareH2Poll submits a single-shot POLL_ADD SQE on the H2 eventfd.
// When handler goroutines write the eventfd, the CQE wakes the ring
// event-driven, replacing the 100μs polling timeout.
// Reports whether the arm was placed. A full SQ ring must NOT be swallowed:
// the poll is single-shot and w.h2PollArmed is never cleared anywhere else,
// so a dropped arm leaves the eventfd deaf for the life of the worker.
// Callers assign the result to w.h2PollArmed; the loop retries via
// rearmH2PollIfPending.
func (w *Worker) prepareH2Poll() bool {
	// No descriptor, nothing to arm — and nothing to retry either, so the
	// rearm flag stays clear. Since celeris#655 the handle reports -1 both
	// before the eventfd is created and after shutdown closed it, where the
	// raw field could only be a live number; encodeUserData would smear that
	// -1 across the fd bits (op | uint64(fd)&fdMask) and alias a decode.
	// Every other FD() consumer already guards; this one was the exception.
	efd := w.wakeFD.FD()
	if efd < 0 {
		w.h2PollRearmPending = false
		return false
	}
	sqe := w.ring.GetSQE()
	if sqe == nil {
		w.h2PollRearmPending = true
		return false
	}
	prepPollAdd(sqe, efd, unix.POLLIN)
	setSQEUserData(sqe, encodeUserData(udH2Wakeup, efd))
	w.h2PollRearmPending = false
	return true
}

// rearmH2PollIfPending re-issues an H2 eventfd poll that prepareH2Poll had
// to drop on a full SQ ring. No-op unless a drop is pending.
func (w *Worker) rearmH2PollIfPending() {
	if !w.h2PollRearmPending || w.h2PollArmed || w.wakeFD.FD() < 0 {
		return
	}
	w.h2PollArmed = w.prepareH2Poll()
}

// handleH2Wakeup drains the eventfd counter and re-arms the poll.
// The actual H2 write queue drain happens in the existing bottom-of-loop pass.
func (w *Worker) handleH2Wakeup() {
	var buf [8]byte
	_, _ = unix.Read(w.wakeFD.FD(), buf[:])
	w.h2PollArmed = w.prepareH2Poll()
}

// prepareAccept submits an accept SQE using the best available mode.
func (w *Worker) prepareAccept() {
	sqe := w.ring.GetSQE()
	if sqe == nil {
		// SQ ring full: remember, the loop retries next iteration once
		// Submit has drained the ring (see acceptRearmPending).
		w.acceptRearmPending = true
		return
	}
	w.acceptRearmPending = false
	if w.fixedFiles {
		prepMultishotAcceptDirect(sqe, w.listenFD)
	} else if w.tier.SupportsMultishotAccept() {
		prepMultishotAccept(sqe, w.listenFD)
	} else {
		prepAccept(sqe, w.listenFD, 0)
	}
	setSQEUserData(sqe, encodeUserData(udAccept, w.listenFD))
}

// rearmAcceptIfPending re-issues an accept arm that was dropped by
// prepareAccept on a full SQ ring. No-op unless a drop is pending and the
// worker still owns an open listen socket.
//
// A paused worker whose listener is still open is lingering (celeris#662):
// the kernel is promoting the connections TCP_DEFER_ACCEPT held back and
// queueing new arrivals on that listener, and only an armed accept takes
// them before the close. So the gate is the listener, not the pause. Once
// the pause's close has cleared listenFD nothing may be armed on the
// descriptor being closed.
func (w *Worker) rearmAcceptIfPending() {
	if !w.acceptRearmPending || w.listenFD < 0 {
		return
	}
	w.prepareAccept()
}

// prepareRecv submits a recv SQE for cs. Uses multishot recv with
// ring-mapped provided buffers when available; falls back to single-shot
// per-connection buffer recv. Returns true if the SQE was submitted,
// false if the SQ ring was full.
//
// Takes the connState (not fd+gen) so the arm and its kernelInflight /
// recvArmed bookkeeping cannot diverge: every armed recv MUST be counted,
// or the close path would release cs.buf while the kernel still holds a
// write pointer into it (v1.4.15/7beebb9 corruption).
func (w *Worker) prepareRecv(cs *connState, buf []byte) bool {
	// One recv per connection, always. A second recv armed while the first
	// is still in flight points two kernel writes at the same cs.buf, so
	// the later write lands on top of the earlier one's unread bytes and
	// the inbound stream is corrupted mid-frame — a WebSocket parser then
	// finds a frame boundary at the wrong offset (celeris#484). The window
	// is the backpressure pause: the pause submits an ASYNC_CANCEL, and a
	// resume that arrives before that cancel lands used to arm
	// unconditionally. Returning true reports the postcondition every
	// caller actually wants — "a recv is armed for this conn" — so none of
	// them schedules a redundant retry.
	if cs.recvArmed {
		w.recvArm.noteArmDeclined()
		return true
	}
	sqe := w.ring.GetSQE()
	if sqe == nil {
		// The only way cs.needsRecv is ever set: every caller reads the
		// false and defers the arm. Counted so a stall below can be told
		// apart from a stall that never had an arm to owe (celeris#607).
		w.recvArm.noteSQFullRecv()
		return false
	}
	if w.bufRing != nil {
		prepMultishotRecv(sqe, cs.fd, bufRingGroupID, w.fixedFiles)
	} else {
		prepRecv(sqe, cs.fd, buf)
	}
	setSQEUserData(sqe, encodeUserDataGen(udRecv, cs.fd, cs.generation))
	cs.recvArmed = true
	cs.kernelInflight++
	w.noteRecvPlaced(cs)
	return true
}

// pickRecvTarget selects the recv target for the next SQE on cs. When the
// H1 parser is in a partial-body state and there's bodyBuf tail capacity,
// it returns that slice and flags the conn so handleRecv routes the next
// CQE through the direct-body path (bypassing ProcessH1 + cs.buf memcpy).
// Disabled when a provided-buffer ring is in use (multishot recv path owns
// its own buffer lifecycle). Always clears cs.recvIntoBody when the normal
// cs.buf path is picked.
func (w *Worker) pickRecvTarget(cs *connState) []byte {
	cs.recvIntoBody = false
	// Drop any prior body-recv pin: pickRecvTarget is only invoked to arm a
	// NEW recv, which means the previous (single-shot) recv already
	// completed, so no kernel SQE still targets the old bodyBuf array. The
	// body path below re-sets the pin when it arms into bodyBuf again.
	cs.bodyRecvPin = nil
	// Async mode: only a PROMOTED conn hands h1State to the dispatch
	// goroutine, which the worker cannot safely observe NextRecvBuf against.
	// A non-promoted conn (celeris#356 inline-first) runs ProcessH1 on the
	// worker itself (tryInline), so the worker owns h1State exactly as the
	// sync path does and the zero-copy direct-into-bodyBuf recv is safe —
	// gate the bail on cs.asyncPromoted, not blanket w.async.
	if (w.async && cs.asyncPromoted.Load()) || w.bufRing != nil || cs.h1State == nil {
		return cs.buf
	}
	if !w.h1Only && engine.Protocol(cs.protocol.Load()) != engine.HTTP1 {
		return cs.buf
	}
	if b := cs.h1State.NextRecvBuf(); b != nil {
		cs.recvIntoBody = true
		// Pin the bodyBuf backing array (b shares it) so a later
		// conn.CloseH1 → state.bodyBuf=nil cannot let GC reclaim it while
		// this recv SQE is still in flight (#256 body-buffer UAF; see
		// connState.bodyRecvPin). Released by releaseConnState after the
		// pendingRelease window drains.
		cs.bodyRecvPin = b
		return b
	}
	return cs.buf
}

func (w *Worker) drainDetachQueue() {
	// The other deferred hand-off work first: reaps that missed, or found
	// the SQ ring full, while their recv was still armed (celeris#657).
	if len(w.reapRetry) > 0 {
		w.retryReaps()
	}
	if w.detachQPending.Load() == 0 {
		return
	}
	w.detachQMu.Lock()
	w.detachQSpare, w.detachQueue = w.detachQueue, w.detachQSpare[:0]
	w.detachQPending.Store(0)
	w.detachQMu.Unlock()
	for _, cs := range w.detachQSpare {
		// The dispatch goroutine's hand-back of send completions held while
		// its handler ran (celeris#750): applied before anything else acts
		// on the conn, whichever entry this is.
		if len(cs.heldSends) > 0 {
			w.replayHeldSends(cs)
		}
		if cs.detachClosed {
			continue
		}
		// If the dispatch goroutine enqueued this conn because it
		// observed an error or recovered from a panic, it also set
		// asyncClosed. We're on the worker goroutine here — this is
		// where the conn-table teardown has to happen (dispatch
		// goroutine can't touch w.conns or the dirty list safely).
		if cs.asyncClosed.Load() {
			w.closeConn(cs.fd)
			continue
		}
		// #383 reverse (async): the dispatch goroutine reached a clean park
		// boundary while an io_uring→epoll drain was active and handed the conn
		// to us. Finish on the worker thread: dup the fd for epoll, cancel the
		// armed recv, defer the connState release to its terminal CQE, close the
		// original. The goroutine has already exited, so there is no
		// goroutine-vs-release race. If the drain was stopped meanwhile (or the
		// dup fails), finishAsyncTransplant leaves the conn in place and the next
		// recv respawns its goroutine — nothing is lost.
		if cs.transplantPending.Load() {
			cs.transplantPending.Store(false)
			w.finishAsyncTransplant(cs)
			continue
		}
		// One owner per entry (the celeris#527/#657 fd-lifetime rule): act
		// only for the connState that still owns its slot. A conn can be
		// queued twice in one burst: the dispatch goroutine hands back a
		// conn the dirty pass gave up (relinkOwed, celeris#704) at the top of
		// its loop, and in the same asyncInMu section its park boundary can
		// claim the hand-off above and enqueue it again. The entry that
		// hands it off clears its slot and closes its fd, whose number a new
		// socket may hold by the time anything below runs; putting the conn
		// back on the dirty list would arm the recv it was owed on that
		// socket, and take its bytes.
		if cs.fd < 0 || cs.fd >= len(w.conns) || w.conns[cs.fd] != cs {
			continue
		}
		// Dispatch goroutine promoted the conn to H2 via switchToH2Local
		// on the h2c-upgrade path. Finish the worker-owned bits of the
		// swap: arm the H2 eventfd poll (once per worker) and register
		// the fd on the write-queue poll list. The conn stays alive;
		// subsequent recvs dispatch via the inline H2 path.
		if cs.asyncH2Promoted.Load() {
			cs.asyncH2Promoted.Store(false)
			if !w.h2PollArmed && w.wakeFD.FD() >= 0 {
				w.h2PollArmed = w.prepareH2Poll()
			}
			w.h2Conns = append(w.h2Conns, cs.fd)
			w.markDirty(cs)
			continue
		}
		// Async-mode Detach finalisation: OnDetach ran on the
		// dispatch goroutine and cannot touch worker-owned state
		// (w.detachedCount or the ring). It set asyncDetachPending
		// and enqueued cs so we land here on the worker thread and
		// run those mutations safely. Idempotent — the flag is
		// cleared before the bookkeeping so a second drainDetachQueue
		// pass (from the same enqueue burst) is a no-op.
		if cs.asyncDetachPending {
			cs.asyncDetachPending = false
			w.detachedCount++
			cs.detachCounted = true
			if w.detachedConns != nil {
				w.detachedConns.Add(1)
			}
			if !w.h2PollArmed && w.wakeFD.FD() >= 0 {
				w.h2PollArmed = w.prepareH2Poll()
			}
		}
		// Apply pending pause/resume request from the WS middleware.
		// Pause: cancel any in-flight RECV (multishot or single-shot)
		// targeting this fd. Once cancelled, no recv is re-armed until
		// resume is called. Resume: re-arm recv via prepareRecv.
		//
		// The cancel's own CQE is tagged udRecvCancel and routed to
		// handleRecvCancel, which retires cs.recvCancelPending when the
		// cancel turns out to have matched nothing (celeris#596).
		if desired := cs.recvPauseDesired.Load(); desired != cs.recvPaused {
			if desired {
				if sqe := w.ring.GetSQE(); sqe != nil {
					// Cancel the in-flight recv. cs.fd is a fixed-file
					// INDEX when fixed files are on, so cancelling by raw
					// fd would match nothing; match by the recv's
					// user_data instead, which is unambiguous in both
					// modes (v1.5.0 review 2.5). The match MUST include the
					// conn's generation — the recv SQE carries it (review
					// 2.6), so a gen-less target would match nothing. The
					// cancel SQE itself carries the udRecvCancel tag so the
					// dispatcher can see whether it matched anything.
					// Cancel the in-flight recv by its generation-tagged
					// user_data -- ALWAYS, never by raw fd (celeris#482).
					//
					// The fd-keyed form (IORING_ASYNC_CANCEL_FD|CANCEL_ALL)
					// matches EVERY op on the socket, not just the recv: it
					// also kills a SEND that is poll-armed on a full peer
					// buffer. That send's -ECANCELED lands in handleSend /
					// completeSend, which treat any negative result as a
					// fatal I/O error and close a healthy connection
					// mid-write; on the SEND_ZC path the close stalls and
					// the conn leaks as a paused ESTAB socket that never
					// sees the peer's FIN. The two conditions coincide by
					// construction: a send is cancellable precisely when the
					// peer is slow, which is the same backpressure that
					// triggers this pause.
					//
					// user_data is unambiguous in both fixed-file and raw-fd
					// modes (v1.5.0 review 2.5) and MUST include the conn's
					// generation -- the recv SQE carries it (review 2.6).
					// cancelConnOps already uses exactly this form
					// unconditionally.
					//
					// The cancel's own CQE is REPORTED (no
					// CQE_SKIP_SUCCESS) and tagged udRecvCancel rather than
					// the udProvide "drop it" sentinel, because its result
					// is the only thing that can retire recvCancelPending
					// when the cancel misses: with IORING_ASYNC_CANCEL_ALL a
					// cancel that matched nothing completes as res == 0, a
					// SUCCESS, so the suppressed form posted no CQE at all
					// and the count never came back down for the rest of
					// the conn's life (celeris#596). One extra CQE per
					// backpressure pause, off the per-request path entirely.
					prepCancelUserDataReported(sqe, encodeUserDataGen(udRecv, cs.fd, cs.generation))
					setSQEUserData(sqe, encodeUserDataGen(udRecvCancel, cs.fd, cs.generation))
					cs.recvCancelPending++
					cs.pausesApplied++
				}
			} else {
				// The #484 window: the pause's cancel has not landed yet
				// and the middleware already withdrew the pause. Counted
				// BEFORE prepareRecv so the witness is the window itself,
				// not whether the guard declined (celeris#586).
				if cs.recvCancelPending > 0 {
					w.recvArm.noteResumeWhileCancelPending()
					// A cancel that MISSED (the recv completed with data
					// before the cancel ran, or nothing was armed) used to
					// stay counted indefinitely, because its completion was
					// suppressed as a success and dropped as udProvide — so
					// every later resume was counted as if it were inside
					// the window (celeris#596). handleRecvCancel now retires
					// it on that completion, leaving only the resumes that
					// fall between the missed cancel and its CQE. The
					// narrower witness is still a resume with the cancelled
					// recv actually armed: only then can a second arm be
					// placed on top of a kernel-held one.
					if cs.recvArmed {
						w.recvArm.noteResumeWhileRecvInFlight()
					}
				}
				if w.prepareRecv(cs, cs.buf) {
					cs.needsRecv = false
					w.endRecvStall(cs)
				} else {
					cs.needsRecv = true
				}
			}
			cs.recvPaused = desired
		}
		w.markDirty(cs)
	}
	// Drop the strong refs before reusing the array. Truncating to [:0]
	// leaves every *connState in the backing array reachable until some
	// later drain overwrites that slot, so the queue pins its own
	// high-water mark worth of connStates -- each one holding its buffers
	// and, for a detached conn, its H1State and whatever the middleware
	// hung off it. drainPendingRelease already guards the same hazard.
	clear(w.detachQSpare)
	w.detachQSpare = w.detachQSpare[:0]
}

// releaseDetachedCount gives back this conn's contribution to
// w.detachedCount, if it ever made one.
//
// Keyed on cs.detachCounted, which is set at the two sites that actually bump
// the counter — NOT inferred from h1State.Detached, which is what went wrong.
// In async mode the dispatch goroutine sets Detached in OnDetach while the
// increment is deferred to drainDetachQueue, so a close landing in that window
// decremented for a conn that had never counted; drainDetachQueue then skipped
// its increment on the detachClosed guard, making the loss permanent
// (celeris#549). detachedCount gates the idle sweep, so drifting it toward
// zero silently removes idle enforcement from detached WS/SSE conns.
//
// Clearing the flag makes it idempotent: a second close cannot double-count.
// Worker-thread only, like both increment sites.
func (w *Worker) releaseDetachedCount(cs *connState) {
	if !cs.detachCounted {
		return
	}
	cs.detachCounted = false
	if w.detachedCount > 0 {
		w.detachedCount--
		if w.detachedConns != nil {
			w.detachedConns.Add(-1)
		}
	}
}

func (w *Worker) markDirty(cs *connState) {
	if cs.dirty {
		return
	}
	cs.dirty = true
	cs.dirtyNext = w.dirtyHead
	cs.dirtyPrev = nil
	if w.dirtyHead != nil {
		w.dirtyHead.dirtyPrev = cs
	}
	w.dirtyHead = cs
}

func (w *Worker) removeH2Conn(fd int) {
	for i, f := range w.h2Conns {
		if f == fd {
			w.h2Conns[i] = w.h2Conns[len(w.h2Conns)-1]
			w.h2Conns = w.h2Conns[:len(w.h2Conns)-1]
			return
		}
	}
}

func (w *Worker) removeDirty(cs *connState) {
	// Leaving the dirty list ends any open stall episode, whichever way
	// it ends: the arm landed, the conn paused, or the conn is being torn
	// down (closeConn removes it from the list). Nothing revisits the
	// conn afterwards, so an episode left open here would never be timed
	// at all and the worst case — the stall that outlived its connection
	// — is exactly the one that must not go missing (celeris#607).
	w.endRecvStall(cs)
	if !cs.dirty {
		return
	}
	cs.dirty = false
	if cs.dirtyPrev != nil {
		cs.dirtyPrev.dirtyNext = cs.dirtyNext
	} else {
		w.dirtyHead = cs.dirtyNext
	}
	if cs.dirtyNext != nil {
		cs.dirtyNext.dirtyPrev = cs.dirtyPrev
	}
	cs.dirtyNext = nil
	cs.dirtyPrev = nil
}

// flushDirty is the event loop's dirty-list pass, run once per iteration
// after drainDetachQueue and the driver actions: retry pending sends and
// dropped recv arms on dirty connections (SQ ring was full earlier).
// Typically empty under normal load. Worker thread only.
func (w *Worker) flushDirty() {
	for cs := w.dirtyHead; cs != nil; {
		next := cs.dirtyNext
		if len(cs.heldSends) > 0 {
			// A send completion of this conn is held for its dispatch
			// goroutine (celeris#750), so cs.sending (or zcNotifPending)
			// stays set, and nothing is sent or armed for the conn until the
			// goroutine's hand-back applies the completion; the hand-back
			// puts the conn on this list again (drainDetachQueue). Kept
			// listed until then, it held the ring at a zero wait, a spin,
			// for as long as the handler ran, where a blocking Lock had
			// parked the worker. Give it up, as the celeris#704 give-up
			// below does. Here rather than where the completion is held:
			// the hand-back's own entry lists the conn again when the
			// goroutine is already in its next handler and the completion
			// is held again.
			w.removeDirty(cs)
			cs = next
			continue
		}
		if cs.sending {
			// celeris#607 witness. The retry below is gated on the
			// send, so a connection that is owed a recv arm and has a
			// SEND outstanding is passed over entirely — for as long
			// as the send stays outstanding, which under a slow peer
			// is seconds. Time the episode, once, on the way in.
			if cs.needsRecv && !cs.recvPaused && !cs.recvArmed {
				w.beginRecvStall(cs)
			}
		} else {
			if mu := cs.detachMu; mu != nil && !mu.TryLock() {
				if dispatchBusy(cs, &cs.relinkOwed) {
					// The conn's dispatch goroutine holds detachMu across a
					// user handler (celeris#704): waiting here parked the
					// worker, and every connection of its ring, until the
					// handler returned, and retrying it every pass would
					// hold the ring at a zero wait, a spin, for as long. So
					// give the conn up until the goroutine hands it back: it
					// does so at the top of its next loop, after its own
					// flush of what the handler wrote, and drainDetachQueue
					// puts it on this list again. What the pass owed it (a
					// flush, a recv arm the SQ ring dropped) waits for that,
					// as it waited for the lock before.
					w.removeDirty(cs)
					cs = next
					continue
				}
				mu.Lock()
			}
			sqFull := w.flushSend(cs)
			// The recvArmed check keeps pickRecvTarget out of the
			// picture while a recv is in flight: it MUTATES
			// cs.recvIntoBody, so calling it for an arm that
			// prepareRecv is going to decline would mis-route the
			// in-flight recv's CQE through the direct-body path.
			if cs.needsRecv && !cs.recvPaused && cs.recvArmed {
				cs.needsRecv = false
			} else if cs.needsRecv && !cs.recvPaused {
				// Arm via pickRecvTarget so a deferred BODY recv re-arms
				// into the H1 bodyBuf with cs.recvIntoBody set, instead of
				// blindly re-arming into cs.buf. Re-arming into cs.buf
				// while recvIntoBody stayed true made handleRecv take the
				// body branch on a cs.buf-sized CQE and corrupt the body
				// (v1.5.0 review 2.4). pickRecvTarget MUTATES recvIntoBody,
				// so call it exactly once per arm; it is idempotent across
				// SQ-full retries because NextRecvBuf returns the same tail
				// while bodyBuf state is unchanged.
				if w.prepareRecv(cs, w.pickRecvTarget(cs)) {
					cs.needsRecv = false
				}
			}
			canRemove := !sqFull && len(cs.sendBuf) == 0 && len(cs.writeBuf) == 0 && (!cs.needsRecv || cs.recvPaused)
			if !cs.needsRecv || cs.recvPaused || cs.recvArmed {
				w.endRecvStall(cs)
			}
			if mu := cs.detachMu; mu != nil {
				mu.Unlock()
			}
			if canRemove {
				w.removeDirty(cs)
			}
		}
		cs = next
	}
}

// flushSend submits one SEND SQE for pending data on this connection.
// Only one SEND is in-flight per connection at a time; if a send is already
// in progress, this is a no-op and the next send will be triggered when the
// current one completes.
//
// Double-buffer strategy: writeBuf accumulates handler writes; sendBuf holds
// data the kernel is currently processing. On flush, writeBuf is swapped into
// sendBuf, and the old sendBuf's capacity is reused for the next writeBuf.
//
// Returns true if data is still pending and needs retry (SQ ring was full).
// The caller should markDirty only when this returns true.
func (w *Worker) flushSend(cs *connState) bool {
	if cs.sending || cs.zcNotifPending {
		return false // send in-flight; handleSend/NOTIF will pick up writeBuf
	}

	// If sendBuf still has data (partial send remainder), re-send it.
	if len(cs.sendBuf) > 0 {
		sqe := w.ring.GetSQE()
		if sqe == nil {
			return true // SQ ring full — caller should markDirty
		}
		w.prepSendSQE(sqe, cs, false)
		setSQEUserData(sqe, encodeUserDataGen(udSend, cs.fd, cs.generation))
		cs.sending = true
		cs.kernelInflight++
		w.sendsPending = true
		return false
	}

	// No in-flight data; swap writeBuf → sendBuf if there's new data.
	if len(cs.writeBuf) == 0 && len(cs.bodyBuf) == 0 {
		return false
	}

	cs.sendBuf, cs.writeBuf = cs.writeBuf, cs.sendBuf[:0]

	// Scatter-gather path: a large body was staged via writeBody without
	// being copied into writeBuf. Emit one WRITEV SQE that reads [headers,
	// body] straight to the socket, saving one body-sized memcpy.
	if len(cs.bodyBuf) > 0 {
		sqe := w.ring.GetSQE()
		if sqe == nil {
			cs.writeBuf, cs.sendBuf = cs.sendBuf, cs.writeBuf
			return true
		}
		cs.sendBody = cs.bodyBuf
		cs.bodyBuf = nil
		n := 0
		if len(cs.sendBuf) > 0 {
			cs.iov[n].Base = uintptr(unsafe.Pointer(&cs.sendBuf[0]))
			cs.iov[n].Len = uint64(len(cs.sendBuf))
			n++
		}
		cs.iov[n].Base = uintptr(unsafe.Pointer(&cs.sendBody[0]))
		cs.iov[n].Len = uint64(len(cs.sendBody))
		n++
		prepWritev(sqe, cs.fd, unsafe.Pointer(&cs.iov[0]), n, false)
		cs.sendIsZC = false // WRITEV is never zero-copy (celeris#609)
		if cs.fixedFile {
			setSQEFixedFile(sqe)
		}
		setSQEUserData(sqe, encodeUserDataGen(udSend, cs.fd, cs.generation))
		cs.sending = true
		cs.kernelInflight++
		w.sendsPending = true
		return false
	}

	sqe := w.ring.GetSQE()
	if sqe == nil {
		// SQ ring full — swap back; caller should markDirty.
		cs.writeBuf, cs.sendBuf = cs.sendBuf, cs.writeBuf
		return true
	}
	w.prepSendSQE(sqe, cs, false)
	setSQEUserData(sqe, encodeUserDataGen(udSend, cs.fd, cs.generation))
	cs.sending = true
	cs.kernelInflight++
	w.sendsPending = true
	return false
}

// prepSendSQE prepares a SEND or SEND_ZC SQE based on worker capabilities.
// SEND_ZC is only used for unlinked sends at or above sendZCMinBytes (the
// notification CQE would break the link chain, and on small payloads its extra
// completion costs more than the avoided memcpy). Smaller and linked sends use
// regular SEND.
func (w *Worker) prepSendSQE(sqe unsafe.Pointer, cs *connState, linked bool) {
	// Record the provenance of THIS send before arming it. The completion
	// classifier reads cs.sendIsZC, never w.sendZC, because the fallbacks
	// clear w.sendZC while sends armed under it are still in flight
	// (celeris#609).
	//
	// A worker holding zcHoldBytesMax of send buffers past the release
	// backstop arms no new SEND_ZC until those holds end: that is what bounds
	// them (celeris#812, see zc_send_buffer.go).
	cs.sendIsZC = useSendZC(w.sendZC && w.zcHoldBytes < zcHoldBytesMax, linked, len(cs.sendBuf))
	if cs.sendIsZC {
		// celeris#591 exposure witnesses. Deliberately inside the ZC arm:
		// every sub-sendZCMinBytes and every linked send — the per-request
		// hot path — falls to the plain-SEND branches below and pays
		// nothing. The detached split is keyed on h1State.Detached because
		// a ZC send on a detached conn is the one that can race the
		// dispatch goroutine's inline unix.Write (celeris#587); h1State is
		// nil for a driver/EventLoopProvider conn.
		w.zc.noteSubmit()
		validation.IouringSendZCSubmits.Add(1)
		if cs.h1State != nil && cs.h1State.Detached.Load() {
			validation.IouringSendZCSubmitsDetached.Add(1)
		}
		if cs.fixedFile {
			prepSendZCFixed(sqe, cs.fd, cs.sendBuf, false)
		} else {
			prepSendZC(sqe, cs.fd, cs.sendBuf, false)
		}
	} else if cs.fixedFile {
		prepSendFixed(sqe, cs.fd, cs.sendBuf, linked)
	} else {
		prepSendPlain(sqe, cs.fd, cs.sendBuf, linked)
	}
}

// useSendZC decides whether a send should use zero-copy. ZC is only viable for
// unlinked sends whose payload is large enough that the saved memcpy outweighs
// the extra NOTIF CQE (see sendZCMinBytes).
func useSendZC(sendZC, linked bool, n int) bool {
	return sendZC && !linked && n >= sendZCMinBytes
}

// flushSendLink is like flushSend but links a RECV SQE after the SEND using
// IOSQE_IO_LINK. The kernel chains the operations: when SEND completes, RECV
// starts automatically without another io_uring_enter. This eliminates one
// loop iteration between request/response cycles.
//
// Only used for single-shot recv (bufRing == nil) on the normal request path.
// Falls back to plain (unlinked) SEND if only one SQE slot is available.
func (w *Worker) flushSendLink(cs *connState) bool {
	if cs.sending || cs.zcNotifPending {
		return false
	}

	// Never chain on a DETACHED connection (celeris#607).
	//
	// IOSQE_IO_LINK is an ordering constraint: the kernel does not start
	// the RECV until the SEND completes. On the H1 request/response cycle
	// that costs nothing, because the peer does not send the next request
	// until it has read this response — the two directions alternate by
	// protocol, so ordering them changes nothing.
	//
	// A detached connection (WebSocket / SSE) has no such alternation. Its
	// two directions are independent streams, and a peer is free to keep
	// sending while it stops reading — which is exactly what backpressure
	// IS. The send then blocks on the peer's closed receive window, and
	// because the recv is chained behind it the connection cannot read at
	// all for as long as that lasts: measured at up to 14 s here, with the
	// peer's own bytes piling up unread in the server's receive queue and
	// cs.recvArmed true the whole time, so no other arming site will place
	// a second recv (and it must not — that is celeris#484). The peer's
	// Close frame sits in that queue and the close handshake times out.
	//
	// Unchained, the detached path becomes exactly what the provided-buffer
	// path has always been: flushSend here, and the caller's own
	// `!cqeHasMore && !cs.recvLinked && !cs.recvPaused` tail arms the recv
	// independently — the same standalone arm that runs today whenever the
	// ring has only one free SQE. The syscall count is unchanged either
	// way: both SQEs go to the kernel in the same io_uring_enter, so what
	// the chain buys is ordering, and on H1 that ordering is worth having
	// (the recv starts only once the peer could plausibly have sent, so it
	// tends to find data rather than arm a poll). A detached conn gets no
	// such benefit and pays the liveness cost, so it does not chain.
	if cs.detachMu != nil && cs.h1State != nil && cs.h1State.Detached.Load() {
		return w.flushSend(cs)
	}

	// Partial send remainder — no linking (RECV may already be in flight).
	if len(cs.sendBuf) > 0 {
		return w.flushSend(cs)
	}

	// Scatter-gather body is staged — bypass the SEND→RECV link chain
	// because IORING_OP_WRITEV carries two iovec entries and the normal
	// link machinery in this function only wires up a single SEND SQE.
	// flushSend handles WRITEV correctly; the re-arm of multishot recv
	// is handled by the handleRecv tail on the next CQE.
	if len(cs.bodyBuf) > 0 {
		return w.flushSend(cs)
	}

	if len(cs.writeBuf) == 0 {
		return false
	}

	cs.sendBuf, cs.writeBuf = cs.writeBuf, cs.sendBuf[:0]

	sqe := w.ring.GetSQE()
	if sqe == nil {
		cs.writeBuf, cs.sendBuf = cs.sendBuf, cs.writeBuf
		return true
	}

	// SEND_ZC cannot be linked (notification CQE breaks the link chain).
	// For linked SEND→RECV, always use regular SEND.
	// Try to get a second SQE for the linked RECV.
	recvSQE := w.ring.GetSQE()
	if recvSQE != nil {
		// Link SEND → RECV (always regular SEND, never ZC).
		cs.sendIsZC = false // celeris#609 provenance: linked sends are plain
		if cs.fixedFile {
			prepSendFixed(sqe, cs.fd, cs.sendBuf, true)
		} else {
			prepSendPlain(sqe, cs.fd, cs.sendBuf, true)
		}
		setSQEUserData(sqe, encodeUserDataGen(udSend, cs.fd, cs.generation))
		prepRecv(recvSQE, cs.fd, cs.buf)
		setSQEUserData(recvSQE, encodeUserDataGen(udRecv, cs.fd, cs.generation))
		cs.recvLinked = true
		// celeris#607 witness: stamp when the chain was armed. The recv
		// cannot start before the send completes, so this is the start of
		// the interval during which the connection cannot receive. Stamped
		// from the loop's cached clock and counted into a worker-local
		// batch: this is the per-request send path, and the effect being
		// measured is seconds long, so neither a vDSO call nor an atomic
		// belongs here.
		cs.linkArmedAt = w.cachedNow
		w.linkArmBatch++
		// The linked recv is a kernel-held op like any prepareRecv arm:
		// count it and mark it armed so the close path cancels it and
		// release waits for its terminal CQE (a failed linked SEND makes
		// the kernel post -ECANCELED for it — still a terminal CQE).
		cs.recvArmed = true
		cs.kernelInflight++
		w.noteRecvPlaced(cs)
	} else {
		// Only one SQE slot — unlinked send, can use ZC if available.
		w.prepSendSQE(sqe, cs, false)
		setSQEUserData(sqe, encodeUserDataGen(udSend, cs.fd, cs.generation))
	}
	cs.sending = true
	cs.kernelInflight++
	w.sendsPending = true
	return false
}

// addLiveConn records a new active FD in the dense liveConns slice and
// stamps cs.liveIdx with its position so removeLiveConn can find it in
// O(1). Worker-thread-only (the slice is unsynchronised); called from
// onAcceptedFD. (celeris#318 / v1.5.0 review 1.8)
func (w *Worker) addLiveConn(cs *connState) {
	cs.liveIdx = len(w.liveConns)
	w.liveConns = append(w.liveConns, cs.fd)
	// A dormant sweep judged the set this worker held; it holds a different
	// one now (celeris#657 P9).
	w.wakeSweep()
}

// removeLiveConn removes cs's FD from liveConns in O(1) by swapping the
// last entry into cs.liveIdx and truncating. The swapped-in connState's
// liveIdx is updated to its new position. Worker-thread-only.
// (celeris#318 / v1.5.0 review 1.8)
//
// Callers MUST pass the live connState (not look it up via w.conns[fd]),
// because the close paths nil w.conns[fd] around this call. The defensive
// guards below make a stale / double call a no-op rather than corrupting
// the slice.
func (w *Worker) removeLiveConn(cs *connState) {
	if cs == nil {
		return
	}
	i := cs.liveIdx
	if i < 0 || i >= len(w.liveConns) || w.liveConns[i] != cs.fd {
		// Not actually in liveConns at the recorded index (already removed,
		// never added, or hijacked). Don't touch the slice.
		return
	}
	n := len(w.liveConns) - 1
	if i != n {
		swappedFD := w.liveConns[n]
		w.liveConns[i] = swappedFD
		// Update the swapped-in element's recorded index. Guard against a
		// niled conns slot (shouldn't happen for a live entry, but keeps
		// this robust against teardown ordering).
		if swappedFD >= 0 && swappedFD < len(w.conns) {
			if scs := w.conns[swappedFD]; scs != nil {
				scs.liveIdx = i
			}
		}
	}
	w.liveConns[n] = 0
	w.liveConns = w.liveConns[:n]
	cs.liveIdx = -1
	// The residue this worker last published counted this conn; it no longer
	// holds it (celeris#657 R2).
	w.sweepNoteDeparture()
}

// h1DeadlineSnapshot is the set of cs.h1State fields the two worker-thread
// timeout paths (checkTimeouts and handleHeaderTimer) base their decision on.
type h1DeadlineSnapshot struct {
	haveH1   bool
	detached bool
	idleDL   int64
	hdrDL    int64
}

// snapshotH1Deadlines copies those fields out of cs.h1State under cs.detachMu
// and releases the lock before returning, so the caller can act (closeConn
// takes the same mutex, so holding it across the call would deadlock).
//
// Taking the lock at all is the celeris#548 invariant: the async dispatch
// goroutine's switchToH2Local calls conn.CloseH1(cs.h1State) and then nils
// cs.h1State under this same lock, so testing the pointer and dereferencing
// it again outside the lock is a TOCTOU — the pointer can go nil between the
// two reads and the event loop takes a nil dereference. That invariant is
// stated twice elsewhere in this file and is UNCHANGED here: every read below
// still happens with the lock held. (An atomic shadow copy of the pointer
// would not preserve it — the loser of the race would dereference an H1State
// that CloseH1 has already recycled.)
//
// ok=false means the lock was held by someone else and NOTHING was read: this
// uses TryLock, not Lock (celeris#593). runAsyncHandler holds cs.detachMu
// across the whole of ProcessH1, i.e. for the entire handler call, so a
// blocking Lock on the worker thread parks the LockOSThread'd worker — and
// therefore every other connection it owns — until a slow async handler
// returns. Measured on the celeris#589 rig: with a 300 ms handler on an
// explicitly .Async() route, an unrelated /ping on the same worker was stalled
// for 270 ms of every 300 ms (stalled_frac 0.30-0.33 in 40/40 runs) and 99/99
// stalled samples showed a worker goroutine in checkTimeouts →
// sync.Mutex.Lock; epoll scored 0/40 because its sweep reads h1State without
// a lock.
//
// Skipping the connection for this pass is correct, not merely cheaper.
// detachMu is held exactly while the dispatch goroutine is inside the handler
// (or inside a guarded egress write) — i.e. while the connection is demonstrably
// active — and none of the deadlines the callers evaluate govern a request that
// is executing: the idle and read deadlines are measured between requests, and
// the header deadline is only non-zero while the connection is waiting for a
// request line, which is not a moment at which a handler can hold the lock.
// The sweep re-runs every ~50-100 ms (the 0x1F/0x3FF gate in run()), so the
// worst case is one sweep period of extra timeout latency on a connection that
// is by definition not idle. A connection with no detachMu is read directly.
func snapshotH1Deadlines(cs *connState) (snap h1DeadlineSnapshot, ok bool) {
	mu := cs.detachMu
	if mu != nil {
		if !mu.TryLock() {
			return snap, false
		}
		defer mu.Unlock()
	}
	if h1 := cs.h1State; h1 != nil {
		snap.haveH1 = true
		snap.detached = h1.Detached.Load()
		snap.idleDL = h1.IdleDeadlineNs.Load()
		snap.hdrDL = h1.HeaderDeadlineNs.Load()
	}
	return snap, true
}

// checkTimeouts scans active connections and closes any that have exceeded
// their configured timeout. Called every 1024 iterations (~100ms). This
// replaces the timer wheel: instead of allocating entries and updating maps
// on every recv/send, we store a single lastActivity timestamp on the
// connState and scan here.
//
// Iterates the dense liveConns slice (celeris#318) rather than the sparse
// 0..maxFD range. The previous O(maxFD) scan was the dominant cost above
// ~8 Ki conns on a 16-core box; with liveConns the scan cost is O(active
// conns) regardless of FD space.
func (w *Worker) checkTimeouts() {
	now := time.Now().UnixNano()
	// Drain the deferred-release queue here too. drainPendingRelease gates on
	// w.cachedNow, which is otherwise only refreshed inside the CQE-processing
	// block; on a fully idle worker (no CQEs) cachedNow never advances and
	// closed connStates would be pinned indefinitely. checkTimeouts runs on
	// the ~100ms idle cadence, so refreshing cachedNow and draining here
	// bounds the hold to the wall-clock window even with zero CQE traffic.
	// NOTE: the enqueue-time stamp stays a fresh time.Now()+hold (see
	// queuePendingRelease) so the hold window remains >= cancellation latency
	// regardless of cachedNow staleness (v1.5.0 review 2.9).
	w.cachedNow = now
	if len(w.pendingRelease) > 0 {
		w.drainPendingRelease()
	}
	// Iterate in REVERSE by index: closeConn → finishClose → removeLiveConn
	// swap-removes the FD with the last live entry and shrinks the slice. A
	// forward `range` would then (a) skip the swapped-in conn (moved into an
	// already-visited slot) and (b) read a stale/zeroed tail slot as fd 0,
	// dereferencing w.conns[0]. Reverse-by-index visits each conn exactly
	// once even as entries are swap-removed from the tail (v1.5.0 review 1.9).
	for i := len(w.liveConns) - 1; i >= 0; i-- {
		fd := w.liveConns[i]
		cs := w.conns[fd]
		if cs == nil {
			continue
		}
		if recvStallProbeActive {
			w.reportRecvSilence(cs, now)
		}
		// Deferred close (closeConn): the fd stays open until the queued SENDs
		// complete, which normally happens within a loop pass or two. Nothing
		// else reaps such a conn, so bound the wait — a peer that has stopped
		// reading never completes the SEND at all (celeris#498). The remaining
		// timeouts below are meaningless here: the handler is gone, so there is
		// no read to time out and no new bytes can join the queue.
		// A conn held for a hand-off that neither happened nor was released
		// (celeris#657): arm its recv. Counted; must stay 0.
		if cs.transplantHold && !cs.closing {
			w.rescueHold(cs)
		}
		if cs.closing {
			if now-cs.lastActivity > w.closingDrainBound() {
				// Everything closeConn does before deferring (detach
				// signalling, CloseH1, detachedCount) has already run, so
				// finish exactly where completeSend would have. removeDirty
				// first: finishClose does not unlink cs, and because the SEND
				// is cancelled rather than completed, cs.sending is never
				// cleared — the dirty-list loop skips a sending conn, so it
				// would never unlink it either and the connState would leak
				// through dirtyHead.
				//
				// A send completion held for the dispatch goroutine
				// (celeris#750) is applied first (celeris#880): the teardown
				// reads the conn's send state for what the kernel still owes
				// (fdOwed, zcSendOwed), and a held completion has not changed
				// it yet. Held again, the goroutine is back in a handler and
				// owes a hand-back, whose drain applies it; this pass leaves
				// the conn for the next sweep. Under no lock: handleSend
				// takes detachMu itself, or holds the completion again.
				if len(cs.heldSends) > 0 {
					w.replayHeldSends(cs)
					if w.conns[fd] != cs {
						continue // the completion finished the close
					}
					if len(cs.heldSends) > 0 {
						continue
					}
					// The replay is also what tells the drain's clock the
					// peer is reading (completeSend restamps lastActivity
					// for a closing conn's partial send, celeris#761), and
					// it may have placed the rest of the response: read the
					// bound again, or a send that is making progress is
					// cancelled.
					if now-cs.lastActivity <= w.closingDrainBound() {
						continue
					}
				}
				w.removeDirty(cs)
				w.finishCloseAny(fd, cs)
			}
			continue
		}
		// A conn whose protocol is not detected yet has no H1 state, so the
		// snapshot below cannot see its header deadline. Nor does it have a
		// dispatch goroutine yet (the first bytes decide that), so
		// cs.detected and the deadline are the worker's own and need no lock
		// (celeris#974).
		if !cs.detected && cs.detectDeadline > 0 && now > cs.detectDeadline {
			w.closeConn(fd)
			continue
		}
		// Detached connections (e.g. WebSocket): honor an explicit deadline
		// supplied by the middleware via SetWSIdleDeadline. Skip the
		// engine-config-driven timeouts since the middleware owns the
		// I/O lifecycle. Async-mode conns set detachMu up front without
		// a real detach — fall through to the normal timeout scan for
		// those.
		// Snapshot h1State under detachMu (celeris#548), with TryLock so a
		// slow async handler holding that lock across ProcessH1 cannot pin
		// this worker (celeris#593). Both rules, and why skipping is the
		// correct outcome, are on snapshotH1Deadlines.
		snap, ok := snapshotH1Deadlines(cs)
		if !ok {
			// detachMu held: the conn is inside its handler, so none of the
			// deadlines below apply to it right now. Re-examined next sweep.
			continue
		}
		h1Detached, idleDL, hdrDL := snap.detached, snap.idleDL, snap.hdrDL

		// engine-config-driven timeouts do not apply to a detached conn —
		// the middleware owns its I/O lifecycle.
		if h1Detached {
			if idleDL > 0 && now > idleDL {
				w.closeConn(fd)
			}
			continue
		}
		// ReadHeaderTimeout: slowloris defence. Plain unix.Close path
		// (handled by closeConn → finishCloseDetached fastClose branch).
		// See handleHeaderTimer for the rationale (mirrors net/http).
		if hdrDL > 0 && now > hdrDL {
			w.closeConn(fd)
			continue
		}
		elapsed := time.Duration(now - cs.lastActivity)
		if cs.dirty || cs.sending {
			if w.cfg.WriteTimeout > 0 && elapsed > w.cfg.WriteTimeout {
				w.closeConn(fd)
			}
		} else {
			if w.cfg.IdleTimeout > 0 && elapsed > w.cfg.IdleTimeout {
				w.closeConn(fd)
			} else if w.cfg.ReadTimeout > 0 && elapsed > w.cfg.ReadTimeout {
				w.closeConn(fd)
			}
		}
	}
}

// h2PoolDrainFloor is the least time the worker, its context cancelled,
// waits for the HTTP/2 stream handlers on the shared worker pool before it
// shuts down (celeris#759); a live budget of the last Engine.Shutdown extends
// it (h2PoolSettled). h2PoolDrainPoll caps one wait meanwhile.
const (
	h2PoolDrainFloor = 250 * time.Millisecond
	h2PoolDrainPoll  = 10 * time.Millisecond
)

// h2PoolSettled reports whether the worker, its context cancelled, may go on
// to its send drain and shutdown as far as HTTP/2 is concerned (celeris#759).
// A stream on an async route runs its handler on the shared worker pool, off
// this loop, and its response comes back through the conn's write queue,
// which only this loop drains; shutdown cancelled such streams and closed
// their conns under their handlers, so the client got unexpected EOF, and
// the hooks ran before the handlers had finished. So the loop keeps turning,
// reading and writing as usual on the conns it has (it accepts no new one:
// stopAccepting), until no HTTP/2 conn has a pool handler running, a response
// still in its write queue, or response DATA waiting for the client's
// WINDOW_UPDATE. Every HTTP/2 conn is sent GOAWAY first, so its client opens
// no new stream on it, as net/http's graceful shutdown does, and a stream it
// opens anyway is refused. The wait is bounded by the budget the last
// Engine.Shutdown handed over, as epoll's is, and never ends before
// h2PoolDrainFloor. Worker thread.
func (w *Worker) h2PoolSettled() bool {
	if len(w.h2Conns) == 0 {
		return true
	}
	now := time.Now().UnixNano()
	if w.h2DrainStart == 0 {
		w.h2DrainStart = now
	}
	busy := false
	for _, fd := range w.h2Conns {
		cs := w.conns[fd]
		if cs == nil || cs.h2State == nil {
			continue
		}
		if !cs.h2GoAwaySent {
			mu := cs.detachMu
			if mu != nil {
				mu.Lock()
			}
			cs.h2GoAwaySent = cs.h2State.GoAway(cs.writeFn)
			if cs.h2GoAwaySent && w.flushSend(cs) {
				w.markDirty(cs)
			}
			if mu != nil {
				mu.Unlock()
			}
		}
		if cs.h2State.PoolHandlersRunning() || cs.h2State.WriteQueuePending() || cs.h2State.OutboundPending() {
			busy = true
		}
	}
	if !busy {
		return true
	}
	end, bounded := w.drainEnd(w.h2DrainStart, int64(h2PoolDrainFloor))
	return bounded && now > end
}

// drainEnd is when (UnixNano) a drain that began at start gives up, and false
// while it has no end but the budget's being done. It is bounded as epoll's
// send drain is (epoll's Loop.sendDrainWait), and the HTTP/2 wait and the
// send drain share it so the two agree: while the budget the last
// Engine.Shutdown handed over (drainBudget) is live, until its deadline or,
// for a ctx without one (context.Background, a WithCancel ctx: net/http's
// "wait as long as it takes"), until it is done, no longer than WriteTimeout
// after start when that is set, and never less than floor from start, which
// is all a done budget, or none, gets. The caller works it out again each
// time it asks: the budget can arrive, or end, in the middle of the drain.
// Worker thread.
func (w *Worker) drainEnd(start, floor int64) (end int64, bounded bool) {
	end = start + floor
	if w.drainBudget == nil {
		return end, true
	}
	p := w.drainBudget.Load()
	if p == nil || (*p).Err() != nil {
		return end, true
	}
	d, hasDeadline := (*p).Deadline()
	ext := d.UnixNano()
	if wt := int64(w.cfg.WriteTimeout); wt > 0 && (!hasDeadline || start+wt < ext) {
		ext, hasDeadline = start+wt, true
	}
	if !hasDeadline {
		return 0, false // until the budget is done
	}
	return max(end, ext), true
}

// noteSendDrainGaveUp records that the send drain ran out of time with
// response bytes still queued or in flight, and shutdown is about to close
// those connections: the data loss the bound is. It is counted once per
// worker shutdown (handoffLossStats.shutdownSendDrainGaveUp, not an
// EngineMetrics field) and logged with how many connections and how many
// queued bytes it cost; bytes the kernel had already taken are not counted,
// and the client gets those (celeris#806). waited is how long the drain ran.
func (w *Worker) noteSendDrainGaveUp(waited int64) {
	w.handoffLoss.noteShutdownSendDrainGaveUp()
	if w.logger == nil {
		return
	}
	conns, queued := w.pendingSendLoss()
	w.logger.Warn("io_uring shutdown: the send drain ran out of time; closing connections whose response the peer has not taken",
		"worker", w.id, "conns", conns, "queued_bytes_lost", queued, "waited", time.Duration(waited))
}

// pendingSendLoss counts the connections hasPendingSends finds pending and
// the response bytes they have queued and the socket has not yet taken (staged
// in the conn's buffers or in flight in a SEND). A
// connection whose detachMu is held by a running handler counts as one, its
// bytes unknown (TryLock, as hasPendingSends).
func (w *Worker) pendingSendLoss() (conns, queued int) {
	for _, fd := range w.liveConns {
		cs := w.conns[fd]
		if cs == nil {
			continue
		}
		mu := cs.detachMu
		if mu != nil && !mu.TryLock() {
			conns++
			continue
		}
		if connSendPending(cs) {
			conns++
			queued += len(cs.sendBuf) + len(cs.writeBuf) + len(cs.bodyBuf)
		}
		if mu != nil {
			mu.Unlock()
		}
	}
	return conns, queued
}

// hasPendingSends reports whether any live connection still has response bytes
// queued for the ring or a SEND in flight in the kernel. Called only from the
// shutdown drain in run() (celeris#595), never on the hot path, so the
// O(live conns) scan costs nothing per request.
//
// Conns with detachMu (detached streams AND async-dispatch conns) have their
// buffers written by another goroutine, so they are inspected under that
// mutex — with TryLock, never a blocking Lock: a dispatch goroutine parked
// inside a write must not be able to wedge shutdown. A conn whose mutex is
// held right now counts as pending, which at worst spends the drain window
// on it and then tears down exactly as before.
func (w *Worker) hasPendingSends() bool {
	for _, fd := range w.liveConns {
		cs := w.conns[fd]
		if cs == nil {
			continue
		}
		if mu := cs.detachMu; mu != nil {
			if !mu.TryLock() {
				return true
			}
			pending := connSendPending(cs)
			mu.Unlock()
			if pending {
				return true
			}
			continue
		}
		if connSendPending(cs) {
			return true
		}
	}
	return false
}

// connSendPending reports whether cs has response bytes staged for a SEND or
// a SEND already handed to the kernel. Callers own the synchronisation (see
// hasPendingSends).
func connSendPending(cs *connState) bool {
	return cs.sending || len(cs.sendBuf) > 0 || len(cs.writeBuf) > 0 || len(cs.bodyBuf) > 0
}

// endDetachedWrites ends a detached conn's writes: its recv-end error parked
// for the close, delivered as closeConn delivers it, under the lock and before
// OnDetachClose, or the middleware is never told (celeris#867); then
// detachClosed, after which writeFn no-ops, and OnDetachClose. Called with
// cs.detachMu held, by shutdown and, early, by stopDetachedProducers.
// Idempotent: the parked error and OnDetachClose are consumed once.
func endDetachedWrites(cs *connState) {
	if err := cs.closeErr; err != nil {
		cs.closeErr = nil
		if cs.h1State != nil && cs.h1State.OnError != nil {
			cs.h1State.OnError(err)
		}
	}
	cs.detachClosed = true
	// Acquire barrier — see the primary close path: skip OnDetachClose
	// until the WS upgrade has fully wired the conn (WSReady) to avoid
	// racing the post-Detach wiring on the async goroutine.
	if cs.h1State != nil && cs.h1State.WSReady.Load() && cs.h1State.OnDetachClose != nil {
		cs.h1State.OnDetachClose()
		cs.h1State.OnDetachClose = nil
	}
}

// stopDetachedProducers is the detached half of epoll's shutdown (its phase
// 1, which runs before its send drain), run by the send drain once it is past
// its first shutdownSendDrainNanos (celeris#806): a detached conn (Server-Sent
// Events, a WebSocket) has a goroutine of its own that keeps writing, so,
// with the drain lasting as long as the budget, a stream that never ends
// would keep hasPendingSends true for all of it. After this its writes no-op
// and what is already queued is what the drain finishes. Only conns that have
// detached (h1State.Detached) and whose dispatch goroutine is not inside the
// handler: the goroutine holds detachMu across the handler, so the TryLock
// below fails for a running async handler, which is answered, up to the
// budget, as before. TryLock, never Lock, as hasPendingSends: a goroutine
// parked inside a write must not wedge the loop, and a conn whose lock is busy
// is tried again on the next pass (it counts as pending until then).
//
// What a producer's guarded writeFn left queued is flushed, not stranded: its
// inline write may have taken only part of the bytes (or none: the accepted
// socket is non-blocking), leaving the rest in writeBuf with no SEND in
// flight and the conn on detachQueue, and drainDetachQueue skips a
// detachClosed conn before it would mark the conn dirty. So a conn with bytes
// queued and no SEND (or SEND_ZC notification, whose completion flushes them)
// outstanding is marked dirty here; without that nothing would ever send them,
// and the drain would last the budget although every client reads. A conn
// whose async Detach is not yet finalised (asyncDetachPending: the same
// drainDetachQueue pass that skips a detachClosed conn also counts it) is left
// for that pass and tried again on the next. Worker thread.
func (w *Worker) stopDetachedProducers() {
	for _, fd := range w.liveConns {
		cs := w.conns[fd]
		if cs == nil || cs.detachMu == nil || cs.h1State == nil || !cs.h1State.Detached.Load() {
			continue
		}
		if !cs.detachMu.TryLock() {
			continue
		}
		if !cs.detachClosed && !cs.asyncDetachPending {
			endDetachedWrites(cs)
			if !cs.sending && !cs.zcNotifPending && connSendPending(cs) {
				w.markDirty(cs)
			}
		}
		cs.detachMu.Unlock()
	}
}

func (w *Worker) shutdown() {
	// First: close every adoption still queued, and refuse every AdoptConn
	// from here on. Everything below walks the conn table only, which a
	// queued adoption is not in yet — and closeAdoptQueue must run before
	// the wakeup eventfd is closed, because addAdoptAction signals it under
	// the same lock (celeris#658).
	w.closeAdoptQueue()
	// The fd-lifetime rule at shutdown (celeris#685): end every op the kernel
	// still owes on a connection's descriptor before any of those
	// descriptors is closed (endOwedOpsAtShutdown). Before shutdownDrivers:
	// the drain submits what the SQ ring still holds, and shutdownDrivers
	// closes the driver descriptors on the promise that nothing is
	// submitted after it.
	w.endOwedOpsAtShutdown()
	// Fire onClose for every registered driver conn before tearing down
	// ring/listen fd. Otherwise driver callbacks are silently dropped. Then
	// wait for the driver closes handed off the worker before the shutdown,
	// and fire their onClose (celeris#735): the loop that would have is gone.
	w.shutdownDrivers()
	w.waitDriverCloses()
	// Reverse-by-index for the same reason as checkTimeouts (v1.5.0 review
	// 1.9): any teardown path that swap-removes from liveConns must not cause
	// a forward range to skip a swapped-in conn or read a zeroed tail slot
	// as fd 0.
	for i := len(w.liveConns) - 1; i >= 0; i-- {
		fd := w.liveConns[i]
		cs := w.conns[fd]
		if cs == nil {
			continue
		}
		detached := cs.detachMu != nil
		if detached {
			cs.asyncClosed.Store(true)
			if cs.asyncCond.L != nil {
				cs.asyncInMu.Lock()
				cs.asyncCond.Broadcast()
				cs.asyncInMu.Unlock()
			}
			cs.detachMu.Lock()
			endDetachedWrites(cs)
			cs.detachMu.Unlock()
		}
		trulyDetached := detached && cs.h1State != nil && cs.h1State.Detached.Load()
		if !trulyDetached && cs.h1State != nil {
			// Mirror the closeConn fix: hold detachMu while tearing
			// down h1State so ProcessH1 in runAsyncHandler isn't still
			// reading the fields CloseH1 writes.
			if detached {
				cs.detachMu.Lock()
				// Re-read Detached under the lock (mirrors epoll e100873): skip
				// CloseH1 if the conn detached while we waited, else we recycle
				// a Context/stream the middleware goroutine still uses.
				if !cs.h1State.Detached.Load() {
					conn.CloseH1(cs.h1State)
				}
				cs.detachMu.Unlock()
			} else {
				conn.CloseH1(cs.h1State)
			}
		}
		if cs.h2State != nil {
			conn.CloseH2(cs.h2State)
		}
		// endOwedOpsAtShutdown has ended the ops owed on fd, so no op can
		// resolve the number once it is free (celeris#685).
		if !cs.fixedFile {
			_ = unix.Close(fd)
		}
		// Do NOT releaseConnState here, detached or not. Conns being torn
		// down by shutdown almost always have a recv SQE armed on cs.buf,
		// and the ring is only closed AFTER this loop — recycling cs into
		// the shared connStatePool now could hand cs.buf to a conn on a
		// still-running sibling worker while this ring's kernel side can
		// still write into it (same #256-class UAF, shutdown variant).
		// The conns remain reachable via w.conns until the Worker itself
		// is collected, well after the ring teardown cancels its ops. A
		// SEND_ZC's send buffer is the exception: no cancel or teardown
		// ends the kernel's use of it, so retainZCSendBufsAtShutdown keeps
		// the ones still owed past the Worker (celeris#812).
	}
	// The counters the loop batches per iteration and flushes near the top of
	// the next one: what was counted since that flush (a close queued later in
	// the last iteration, bytes a send completed) would otherwise never reach
	// the engine-wide ones, which are read after shutdown (celeris#874).
	w.flushBatches()
	// celeris#657 R2: this worker is gone, so it must not leave its last
	// cycle's residue standing in the engine-wide gauges. Nothing else
	// retracts it — the sweep does not run after shutdown.
	w.sweepRetract()
	if w.listenFD >= 0 {
		_ = unix.Close(w.listenFD)
	}
	// The worker has left the SO_REUSEPORT group for good. PauseAccept waits
	// for this flag, and a worker that exits in the middle of a pause linger
	// would otherwise leave it false until PauseAccept's own bound ran out
	// (celeris#662).
	w.listenFDClosed.Store(true)
	w.pause.Notify()
	// celeris#655: Close waits for the signals already in flight and turns
	// every later one into a no-op, so no producer — including the dispatch
	// goroutines this function only joins below — can write this descriptor
	// number once it is free to be recycled.
	w.wakeFD.Close()
	// celeris#812: the ring's last word on which SEND_ZC send buffers the
	// kernel is done with. Every buffer it cannot clear by now is kept for
	// the life of the process: nothing would ever say when the kernel lets
	// go of it, and the Worker, which is all that holds it, can be
	// collected once the engine is dropped.
	w.retainZCSendBufsAtShutdown()
	if w.bufRing != nil && w.ring != nil {
		w.bufRing.Close(w.ring)
	}
	if w.ring != nil {
		_ = w.ring.Close()
	}
	// Join dispatch goroutines; they've been signaled via
	// asyncClosed + Broadcast above. Prevents stale-memory races
	// after the engine claims to have stopped.
	w.asyncWG.Wait()
}

// flushBatches publishes the worker-local counters the run loop adds to per
// event and moves into the shared atomics once per iteration (the block after
// the submit in run), for shutdown to call: the loop returns from several
// places before its next flush, and these are all read after shutdown. Keep it
// in step with that block; TestShutdownFlushesEveryBatch874 fails on a *Batch
// field that this misses. Nil-safe like the witnesses it feeds, since a
// hand-built Worker reaches shutdown. Worker thread only.
func (w *Worker) flushBatches() {
	if w.reqBatch > 0 {
		if w.reqCount != nil {
			w.reqCount.Add(w.reqBatch)
		}
		w.reqBatch = 0
	}
	if w.bytesReadBatch > 0 {
		if w.bytesRead != nil {
			w.bytesRead.Add(w.bytesReadBatch)
		}
		w.bytesReadBatch = 0
	}
	if w.bytesWrittenBatch > 0 {
		if w.bytesWritten != nil {
			w.bytesWritten.Add(w.bytesWrittenBatch)
		}
		w.bytesWrittenBatch = 0
	}
	if w.ringBytesBatch > 0 {
		w.zc.noteRingBytes(w.ringBytesBatch)
		w.ringBytesBatch = 0
	}
	if w.closeFDDeferredBatch > 0 {
		if w.handoffLoss != nil {
			w.handoffLoss.closeFDDeferred.Add(w.closeFDDeferredBatch)
		}
		w.closeFDDeferredBatch = 0
	}
	if w.linkArmBatch > 0 {
		if w.recvArm != nil {
			w.recvArm.linkedRecvArms.Add(w.linkArmBatch)
		}
		w.linkArmBatch = 0
	}
}

// releaseFailedInit closes what run created for a worker that failed before
// it signalled ready: its listen socket and, when it got that far, its ring,
// buffer ring and H2 eventfd (celeris#656). It runs on the worker's own thread
// while nothing else can reach the worker (Listen publishes e.workers only
// after every worker is ready), so it needs no locks and touches no
// connection state, of which there is none yet. Never call it after ready:
// shutdown owns these descriptors then, and a second close could hit a number
// already reused for a connection. Each field is reset, so a second call is a
// no-op. The ring is closed before the listen socket because Submit does not
// report, on its error path, whether the accept SQE reached the kernel
// (io_uring_enter can consume part of a batch and still fail, which is why
// Submit calls retryPending): closing the ring first drops any reference an
// accept that did get through would hold on the socket's file.
func (w *Worker) releaseFailedInit() {
	if w.bufRing != nil && w.ring != nil {
		w.bufRing.Close(w.ring)
	}
	w.bufRing = nil
	if w.ring != nil {
		_ = w.ring.Close()
		w.ring = nil
	}
	// celeris#655: release through the handle, not the raw number. A worker
	// that failed init may already have handed its *WakeFD to producers;
	// Close waits behind the signals in flight, retires the number before
	// closing, and makes every later Signal a no-op. It is idempotent, so
	// shutdown calling it again is free. Kept in the slot the raw close
	// occupied, so the ring-before-listen-socket order documented above is
	// unchanged.
	w.wakeFD.Close()
	if w.listenFD >= 0 {
		_ = unix.Close(w.listenFD)
		w.listenFD = -1
		// Keep the invariant the running loop maintains (celeris#656): the
		// socket is out of the SO_REUSEPORT group, so the flag PauseAccept
		// polls must say so. Nothing reads it for a worker that failed init —
		// Listen never publishes it in e.workers — but "fd closed, flag false"
		// is a state the field's own doc does not describe.
		w.listenFDClosed.Store(true)
	}
}

// createListenSocket binds and listens on addr. deferAccept asks for
// TCP_DEFER_ACCEPT (resource.Config.DisableDeferAccept turns it off). A
// pause clears the option on this socket and lingers before it closes it
// (stepAcceptPause), so the option costs a pause time, not connections
// (celeris#662).
func createListenSocket(addr string, deferAccept bool) (int, error) {
	sa, err := parseAddr(addr)
	if err != nil {
		return -1, err
	}

	family := unix.AF_INET
	if _, ok := sa.(*unix.SockaddrInet6); ok {
		family = unix.AF_INET6
	}

	fd, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}

	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("SO_REUSEADDR: %w", err)
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("SO_REUSEPORT: %w", err)
	}

	// TCP_DEFER_ACCEPT: the kernel holds a connection out of the accept queue
	// until its first data arrives, saving a recv arm per idle connection. It
	// also hides that connection from acceptQueuedOnPause, which is why a
	// pause clears it and lingers before the close (celeris#662).
	if deferAccept {
		_ = unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_DEFER_ACCEPT, 1)
	}
	// TCP_FASTOPEN: allow data in SYN packet, saving 1 RTT for TFO-capable clients.
	_ = unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_FASTOPEN, 256)

	if err := bindiag.BindWithRetry(fd, sa); err != nil {
		diag := bindiag.Format(fd, sa)
		_ = unix.Close(fd)
		return -1, fmt.Errorf("bind: %w [%s]", err, diag)
	}
	if err := unix.Listen(fd, 4096); err != nil {
		diag := bindiag.Format(fd, sa)
		_ = unix.Close(fd)
		return -1, fmt.Errorf("listen: %w [%s]", err, diag)
	}

	return fd, nil
}

// listenAddrOf is boundAddr behind a var so a test can make every worker
// fail to report its address (celeris#639).
var listenAddrOf = boundAddr

// newWorkerRing and submitInitialAccept are the worker's ring setup and its
// first submit behind vars, so a test can make either fail for one worker
// after its listen socket exists (celeris#656). The loop's other submits call
// the ring directly.
var (
	newWorkerRing       = NewRingCPU
	submitInitialAccept = (*Ring).Submit
)

func boundAddr(fd int) net.Addr {
	return bindiag.BoundAddr(fd)
}

// sockaddrString formats a peer address as std does; see bindiag.SockaddrString
// (celeris#925).
func sockaddrString(sa unix.Sockaddr) string {
	return bindiag.SockaddrString(sa)
}

func parseAddr(addr string) (unix.Sockaddr, error) {
	host, portStr := "", addr

	// Handle IPv6 bracket notation: [::1]:8080, [::]:8080
	if len(addr) > 0 && addr[0] == '[' {
		closeBracket := -1
		for i := 1; i < len(addr); i++ {
			if addr[i] == ']' {
				closeBracket = i
				break
			}
		}
		if closeBracket < 0 {
			return nil, fmt.Errorf("invalid addr: missing closing bracket: %s", addr)
		}
		host = addr[1:closeBracket]
		if closeBracket+1 < len(addr) && addr[closeBracket+1] == ':' {
			portStr = addr[closeBracket+2:]
		} else {
			return nil, fmt.Errorf("invalid addr: missing port after bracket: %s", addr)
		}
	} else {
		for i := len(addr) - 1; i >= 0; i-- {
			if addr[i] == ':' {
				host = addr[:i]
				portStr = addr[i+1:]
				break
			}
		}
	}

	port := 0
	for _, c := range portStr {
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("invalid port: %s", portStr)
		}
		port = port*10 + int(c-'0')
	}

	if host == "" || host == "0.0.0.0" {
		return &unix.SockaddrInet4{Port: port}, nil
	}

	// IPv6 addresses
	if host == "::" {
		return &unix.SockaddrInet6{Port: port}, nil
	}
	ip := net.ParseIP(host)
	if ip != nil {
		if ip6 := ip.To16(); ip6 != nil && ip.To4() == nil {
			sa := &unix.SockaddrInet6{Port: port}
			copy(sa.Addr[:], ip6)
			return sa, nil
		}
	}

	sa := &unix.SockaddrInet4{Port: port}
	parts := [4]byte{}
	partIdx := 0
	val := 0
	for _, c := range host {
		if c == '.' {
			if partIdx >= 3 {
				return nil, fmt.Errorf("invalid addr: %s", addr)
			}
			parts[partIdx] = byte(val)
			partIdx++
			val = 0
		} else if c >= '0' && c <= '9' {
			val = val*10 + int(c-'0')
		} else {
			return nil, fmt.Errorf("invalid addr: %s", addr)
		}
	}
	parts[partIdx] = byte(val)
	sa.Addr = parts
	return sa, nil
}
