package engine

import (
	"context"
	"net"
	"os"

	"github.com/goceleris/celeris/engine/internal/errclass"
)

// Engine is the interface that all I/O engine implementations must satisfy.
// Implementations include io_uring, epoll, adaptive, and the standard library
// net/http server. Engine methods are safe for concurrent use after Listen is called.
type Engine interface {
	// Listen starts the engine and blocks until ctx is canceled or a fatal
	// error occurs. The engine begins accepting connections on the configured address.
	Listen(ctx context.Context) error
	// Shutdown gracefully drains in-flight connections. The provided context
	// controls the deadline; if it expires, remaining connections are closed.
	Shutdown(ctx context.Context) error
	// Metrics returns a point-in-time snapshot of engine performance counters.
	Metrics() EngineMetrics
	// Type returns the engine type identifier (IOUring, Epoll, Adaptive, or Std).
	Type() EngineType
	// Addr returns the bound listener address, or nil if not yet listening.
	Addr() net.Addr
}

// AcceptController is implemented by engines that support dynamic accept
// control, used by the adaptive engine to pause/resume individual sub-engines
// during switches.
type AcceptController interface {
	// PauseAccept stops accepting new connections. Existing connections continue.
	PauseAccept() error
	// ResumeAccept resumes accepting new connections after a pause.
	ResumeAccept() error
}

// SwitchFreezer is implemented by the adaptive engine to allow external code
// (e.g., benchmarks) to temporarily prevent engine switches.
type SwitchFreezer interface {
	// FreezeSwitching prevents the adaptive engine from switching.
	FreezeSwitching()
	// UnfreezeSwitching allows engine switching to resume.
	UnfreezeSwitching()
}

// SendfileCapable is an optional interface implemented by engines that
// support zero-copy file responses via sendfile(2). The H1 static-file
// response path type-asserts the engine for it; engines that do not
// implement it (iouring, std) fall back to the buffered read+write path.
// See celeris#317. The epoll engine implements it.
//
// The fdOut argument is the engine's per-connection socket FD. The
// caller drives the connState lifecycle (locking, dirty-list membership,
// timeout tracking, EAGAIN resume) — the engine's sendfile path is a
// syscall shim that advances as far as the kernel send buffer allows and
// reports backpressure for the caller to resume.
//
// offset and length describe the slice of the source file to send.
// length ≤ 0 means "send to EOF" (the implementation stats the file and
// uses size-offset). The headers slice, when non-empty, is flushed via
// write(2) before the sendfile loop begins.
//
// Returns the number of BODY bytes sent on this call and an error. A
// short send under kernel-send-buffer pressure surfaces
// EAGAIN/EWOULDBLOCK (via errors.Is) along with the partial body count,
// so the caller defers and resumes; the returned count is never
// corrupted on EAGAIN. EINTR is retried internally. The HEAD-request
// invariant (never send a body) is the caller's responsibility — the
// caller must not invoke Sendfile for a HEAD request.
type SendfileCapable interface {
	Sendfile(fdOut int, file *os.File, offset, length int64, headers []byte) (int64, error)
}

// EngineMetrics is a point-in-time snapshot of engine-level performance
// counters. Each engine maintains internal atomic counters and populates a
// fresh snapshot on each [Engine.Metrics] call.
//
// v1.5.0: LatencyP50 / LatencyP99 / LatencyP999 were removed (celeris#321).
// The fields were declared but never written by any sub-engine (the
// sub-engines don't track per-request latency histograms — they only
// count requests). The adaptive engine's aggregator at
// adaptive/engine.go was the only reader and it received zeros from
// both sub-engines, so removing the fields changes nothing observable.
// SyscallRate, referenced by the issue, never existed in the tree.
type EngineMetrics struct { //nolint:revive // user-approved name
	// RequestCount is the cumulative number of requests handled by this engine.
	RequestCount uint64
	// ActiveConnections is the current number of open connections.
	ActiveConnections int64
	// ErrorCount is the cumulative number of connection-level or protocol
	// errors. It is the SUM of the eleven Error* buckets below and is derived
	// from them rather than counted alongside them, so the total and the
	// parts cannot disagree. Read the buckets to find out what happened;
	// this field only says how much of it there was.
	ErrorCount uint64
	// ErrorAcceptFDLimit is the share of ErrorCount from accepts refused
	// for want of a descriptor: EMFILE (per-process) or ENFILE
	// (system-wide). Populated by epoll and io_uring.
	ErrorAcceptFDLimit uint64
	// ErrorAcceptCancelled is the share of ErrorCount from accept failures
	// that mean the accept went away rather than the host running out of
	// something: ECANCELED (an in-flight accept was cancelled, which is what
	// PauseAccept does), EBADF (the listen descriptor was closed under an
	// accept), ECONNABORTED and EINTR.
	//
	// This is the bucket a PauseAccept lands in, so on the adaptive engine
	// it is a per-switch cost rather than a fault, and it is the one that
	// separates a switch transient from a sustained accept-side loss. Note
	// the asymmetry it exposes: io_uring reports all four through a
	// completion and counts them, while epoll's accept4 loop retries
	// ECONNABORTED and EINTR in place and counts neither — so an epoll
	// column reading 0 next to a nonzero io_uring column on the same
	// refapp is partly this, not a difference in what the two engines
	// suffered.
	ErrorAcceptCancelled uint64
	// ErrorAcceptOther is the share of ErrorCount from accept failures
	// that are neither of the two above.
	ErrorAcceptOther uint64
	// ErrorConnTableCap is the share of ErrorCount from descriptors
	// dropped because they fall outside the worker's flat connection
	// table — the epoll conn-table cap and io_uring's equivalent bound —
	// counted on both the accept path and the transplant adoption path.
	// The descriptor is closed and the connection is lost, so a nonzero
	// value means a worker is at its per-worker connection limit.
	ErrorConnTableCap uint64
	// ErrorConnRegister is the share of ErrorCount from descriptors
	// dropped because registering them with the event loop failed
	// (epoll_ctl ADD), on both the accept and the adoption path. epoll
	// only.
	ErrorConnRegister uint64
	// ErrorListenerRecreate is the share of ErrorCount from failures to
	// re-create a listen socket after a ResumeAccept. The loop or worker
	// that hits one shuts itself down, so a nonzero value is an engine
	// that has permanently lost accept capacity on that worker.
	ErrorListenerRecreate uint64
	// ErrorTransplantAdopt is the share of ErrorCount from adoptions
	// refused because the target's conn-table slot for that descriptor was
	// already occupied. It tracks TransplantAdoptSlotOccupied exactly; the
	// same event is counted in both because it is both a lost hand-off and
	// an error, and celeris#624's hand-off ledger has to stand on its own.
	ErrorTransplantAdopt uint64
	// ErrorSendPeerGone is the share of ErrorCount from send completions
	// that failed because the peer was already gone — EPIPE, ECONNRESET,
	// ECONNABORTED, ENOTCONN. One count per connection whose client
	// stopped reading before its response flushed, so it measures how
	// often clients abandon requests and scales with offered load and
	// client timeouts, not with any server fault.
	//
	// io_uring only, and that asymmetry is the finding behind celeris#645:
	// epoll reports a dead peer through the handler's OnError and has
	// never fed ErrorCount at all, so the SAME abandoned request costs
	// io_uring one ErrorCount and epoll zero. Any epoll-vs-io_uring
	// ErrorCount comparison is dominated by this bucket, and on the
	// adaptive engine it switches on at the promotion — not because
	// anything started failing, but because the sub-engine that counts it
	// started serving.
	ErrorSendPeerGone uint64
	// ErrorSend is the share of ErrorCount from send completions that
	// failed for any other reason — a genuine transmit fault rather than a
	// client that left. io_uring only, for the same reason as
	// ErrorSendPeerGone.
	ErrorSend uint64
	// ErrorRequestBody is the share of ErrorCount from requests rejected
	// before the handler ran because the body would not read or exceeded
	// MaxRequestBodySize. std only.
	ErrorRequestBody uint64
	// ErrorHandler is the share of ErrorCount from handler invocations
	// that returned an error. std only — the native engines do not fold a
	// handler error into ErrorCount.
	ErrorHandler uint64
	// Throughput is the recent requests-per-second rate.
	Throughput float64
	// AsyncRoutes is the count of routes registered with .Async(true) on
	// this engine's handler. Static after Listen — derived from the
	// router's per-route async flags and exposed for diagnostics so
	// operators can see how many handlers run on the per-conn dispatch
	// goroutine vs inline on the worker. Zero on engines whose handler
	// does not implement [github.com/goceleris/celeris/protocol/h2/stream.AsyncRouteResolver].
	AsyncRoutes int
	// AsyncPromotedConns is the cumulative number of connections that
	// have been promoted from inline-on-worker to the per-conn dispatch
	// goroutine via per-handler async (celeris #300). Counts promotions,
	// not currently-promoted conns — every fresh async-mode connection
	// that hits an async route increments this once. Useful to observe
	// how often the inline → goroutine handoff fires vs the pure-sync
	// inline fast path.
	AsyncPromotedConns uint64
	// Workers is the number of I/O workers (io_uring) or event loops
	// (epoll) the engine is running. Static after Listen. The adaptive
	// controller divides ActiveConnections by it to derive the
	// conns-per-worker load signal that drives engine selection — and it
	// reads that pair off the ACTIVE sub-engine, not off the adaptive
	// wrapper, which reports the SUM over both sub-engines (both run at
	// once, so the sum is the divisor matching the summed
	// ActiveConnections). Zero before Listen.
	Workers int
	// AcceptCount is the cumulative number of connections accepted by this
	// engine since start. Together with elapsed time it yields the accept
	// rate (new-connection arrival rate) used as a secondary load signal.
	AcceptCount uint64
	// CloseCount is the cumulative number of connections closed by this
	// engine since start. AcceptCount - CloseCount tracks the live count;
	// a high close rate relative to accepts indicates short-lived
	// churn-style connections.
	CloseCount uint64
	// BytesRead is the cumulative number of payload bytes received from
	// the network across all connections. Used with BytesWritten and
	// RequestCount to derive the average bytes-per-request signal that
	// suppresses io_uring selection for link-bound (large-payload)
	// workloads where the engines tie.
	BytesRead uint64
	// BytesWritten is the cumulative number of payload bytes sent to the
	// network across all connections. See BytesRead.
	BytesWritten uint64
	// AdaptiveSwitches is the cumulative count of completed epoll⇄io_uring
	// switches performed by the adaptive engine. Zero for non-adaptive
	// engines, which never switch. Live switching is the adaptive engine's
	// most complex path; surfacing the count lets ops and benchmarks
	// correlate a throughput or tail-latency anomaly with switching activity
	// (a rare switch transient can skew a single benchmark pass).
	AdaptiveSwitches uint64
	// RecvResumeWhileCancelPending is the cumulative number of times the
	// io_uring engine processed a WebSocket backpressure resume while the
	// pause's ASYNC_CANCEL was still in flight — the exact window in which
	// celeris#484 armed a second recv. Zero on other engines. A load that
	// never moves it has not exercised the #560 guard (celeris#586).
	RecvResumeWhileCancelPending uint64
	// RecvResumeWhileRecvInFlight is the subset of RecvResumeWhileCancelPending
	// in which the cancelled recv was still armed when the resume was
	// processed — the only state in which a second recv could be placed on
	// top of a kernel-held one, and so the witness that the celeris#484
	// window was actually reached. RecvResumeWhileCancelPending also counts
	// the resumes that land between a cancel that missed (the recv completed
	// first, so nothing was cancelled) and that cancel's own completion, so
	// it sits a handful above this one; before celeris#596 a missed cancel
	// was never retired at all and it sat ~1000x above.
	RecvResumeWhileRecvInFlight uint64
	// RecvArmDeclined is the cumulative number of recv arms the io_uring
	// engine declined because a recv was already armed on that connection
	// (the #560 guard, any caller). Zero on other engines.
	RecvArmDeclined uint64
	// RecvDoubleArmed is the cumulative number of times a second recv SQE
	// was placed for one io_uring connection while the first was still
	// outstanding — the celeris#484 defect itself, as bookkept in
	// userspace. Must stay 0. Zero on other engines.
	RecvDoubleArmed uint64
	// RecvCQEUnaccounted is the cumulative number of terminal recv
	// completions the io_uring engine received for a live connection that
	// had no recv outstanding in its bookkeeping — a kernel-held recv the
	// engine lost track of, the only witness of a double recv that the
	// userspace guard cannot see. Must stay 0. Zero on other engines.
	RecvCQEUnaccounted uint64
	// RecvSQFull is the cumulative number of recv arms the io_uring engine
	// could not place because the submission queue had no free entry. It
	// is the only way a connection is ever left owing a recv, so it bounds
	// RecvStallEpisodes from above. Zero on other engines.
	RecvSQFull uint64
	// RecvStallEpisodes is the cumulative number of times the io_uring
	// dirty-list retry passed over a connection that was owed a recv arm
	// because a SEND was still outstanding on it. Counted once per
	// episode, at the transition into it. Zero on other engines.
	RecvStallEpisodes uint64
	// RecvStallNanos is the total wall time those episodes lasted, from
	// the first skipped pass to the arm (or to the connection leaving the
	// dirty list). Zero on other engines.
	RecvStallNanos uint64
	// RecvStallMaxNanos is the longest single such episode. This is the
	// discriminating one: SQ-ring pressure resolving inside a pass is
	// normal, while an episode measured in seconds is a connection that
	// received nothing for seconds with its peer's bytes sitting unread in
	// the kernel (celeris#607). Zero on other engines.
	RecvStallMaxNanos uint64
	// RecvLinkedArms is the cumulative number of times the io_uring engine
	// chained a RECV behind a SEND with IOSQE_IO_LINK (the single-shot
	// request/response fast path). Zero on other engines.
	RecvLinkedArms uint64
	// RecvLinkedBlockedNanos is the total time those chained recvs spent
	// waiting for their send to complete — time the connection could not
	// receive, because the kernel does not start a linked operation until
	// its predecessor finishes. Measured at the send's completion, with
	// the recv provably still queued. Zero on other engines.
	RecvLinkedBlockedNanos uint64
	// RecvLinkedBlockedMaxNanos is the longest single such wait. A
	// request/response cycle pays microseconds; a peer that has stopped
	// reading turns it into seconds, during which the peer's own bytes
	// pile up unread in the server's receive queue (celeris#607). Zero on
	// other engines.
	RecvLinkedBlockedMaxNanos uint64
	// DetachedConnections is the current number of connections handed to a
	// detached middleware goroutine (WebSocket / SSE), summed over the
	// io_uring workers. It mirrors the per-worker detachedCount that gates
	// the idle-deadline sweep cadence, so a drift between this gauge and the
	// number of live detached streams is the celeris#549 accounting bug made
	// visible (celeris#584). Zero on engines that do not keep the count.
	DetachedConnections int64
	// DetachWindowCloses is the cumulative number of io_uring async-mode
	// connections that closed between the middleware's Detach and the
	// worker's deferred detachedCount increment. Each one is a close the
	// pre-#551 accounting decremented without a matching increment; the
	// counter is the exposure proof that the celeris#549 window was entered
	// at all (celeris#584). Zero on other engines.
	DetachWindowCloses uint64
	// ZCSendsSubmitted is the cumulative number of IORING_OP_SEND_ZC SQEs
	// the io_uring engine armed. It is the exposure witness for the
	// zero-copy send path: a benchmark or soak that reports a clean ZC
	// result with this at 0 never ran the branch (celeris#585/#587/#591).
	// One atomic add per ZC submit, inside the ZC arm only — sub-threshold
	// and linked sends (the per-request hot path) add nothing. Zero on
	// other engines and whenever CELERIS_IOURING_SEND_ZC disables ZC.
	ZCSendsSubmitted uint64
	// ZCNotifs is the cumulative number of SEND_ZC notification CQEs
	// (IORING_CQE_F_NOTIF) the io_uring engine processed — the completions
	// that release the kernel-pinned send buffer. ZCSendsSubmitted minus
	// ZCNotifs is the number of ZC sends whose buffer is still pinned, so
	// the pair bounds how long the ZC cycle stayed open. Zero on other
	// engines.
	ZCNotifs uint64
	// InlineBytes is the cumulative number of payload bytes the io_uring
	// engine wrote with a raw unix.Write(2) from a detached middleware
	// goroutine (the WebSocket / SSE inline-egress fast path) instead of
	// through the ring. Those bytes can never be zero-copy, so
	// InlineBytes vs RingBytes is the egress-fabric split the SEND_ZC A/B
	// needs to interpret a throughput delta (celeris#585). Zero on other
	// engines.
	InlineBytes uint64
	// RingBytes is the cumulative number of payload bytes the io_uring
	// engine flushed through ring SEND / SEND_ZC / WRITEV completions —
	// the complement of InlineBytes within BytesWritten. Accumulated in a
	// worker-local counter and published with one atomic per event-loop
	// iteration, exactly like BytesWritten, because this site IS the
	// per-request send path. Zero on other engines.
	RingBytes uint64
	// StandbyActiveConnections is the share of ActiveConnections held by
	// the adaptive engine's STANDBY sub-engine. ActiveConnections stays the
	// sum of both sub-engines (the controller divides it by Workers), so
	// the active engine's own share is ActiveConnections minus this field.
	// After a promotion the standby keeps serving the keep-alives that were
	// established before the switch until the transplant drain moves them,
	// and only the split says which side a live-gauge step came from
	// (celeris#624). Zero on every non-adaptive engine, and zero on an
	// adaptive engine whose lazy standby was never built.
	StandbyActiveConnections int64
	// StandbyCloseCount is the share of CloseCount contributed by the
	// adaptive engine's STANDBY sub-engine, on the same split as
	// StandbyActiveConnections. Zero on every non-adaptive engine.
	StandbyCloseCount uint64
	// StandbyErrorCount is the share of ErrorCount contributed by the
	// adaptive engine's STANDBY sub-engine, on the same split as
	// StandbyActiveConnections — the other half of celeris#645's question.
	// The cause buckets say WHAT went wrong; this says which sub-engine it
	// went wrong on, and the two together are what turn a single adaptive
	// number into "the standby's accepts were cancelled at the promotion"
	// or "the promoted engine is failing sends". Zero on every
	// non-adaptive engine and while the lazy standby is unbuilt.
	StandbyErrorCount uint64
	// TransplantAdopted is the cumulative number of connections this engine
	// has ADOPTED from the other engine through
	// [TransplantTarget.AdoptConn] (#383). The adopting side fires no
	// OnConnect — the connection was already counted when the source engine
	// accepted it — so this counter is the only record of the increment
	// half of a hand-off. On the adaptive engine it is the sum over both
	// sub-engines. Zero on engines that never adopt.
	TransplantAdopted uint64
	// TransplantDetached is the cumulative number of connections this
	// engine has DETACHED for a transplant: dropped from its event loop,
	// live set and conn table, with the fd deliberately left open and
	// NO OnDisconnect fired (#383). TransplantDetached - TransplantAdopted
	// is the number of connections currently in flight between the two
	// sub-engines; on the adaptive engine, where both halves are summed, a
	// residual that never returns to zero is a hand-off that was lost
	// mid-flight and names hypothesis (A) of celeris#624 — the engine's
	// live gauge fell with no hook movement because nothing closed.
	// Zero on engines that never transplant.
	TransplantDetached uint64
	// TransplantAdoptSlotOccupied is the cumulative number of adoptions
	// refused because the target engine's conn-table slot for that
	// descriptor was already occupied. The branch bumps ErrorCount,
	// returns, and deliberately does NOT close the descriptor (the slot
	// holder may close the same number later), so the connection is lost
	// with no close and no hook — silent before this counter existed. It
	// is one of the candidate silent drop points in celeris#624 and must
	// stay 0; a nonzero value IS the answer for that run.
	TransplantAdoptSlotOccupied uint64
	// CloseMissingConnState is the cumulative number of io_uring closes
	// that decremented the live-connection gauge and bumped CloseCount for
	// a descriptor whose connection state was already nil, so the
	// OnDisconnect hook was skipped. That makes the close invisible to
	// every hook-derived counter while the engine gauge moves — hypothesis
	// (B) of celeris#624. Must stay 0; a nonzero value IS the answer for
	// that run. Zero on other engines, whose close paths hold a non-nil
	// connection state by construction.
	CloseMissingConnState uint64
	// TransplantHandoffRefused is the cumulative number of hand-offs in
	// which the SOURCE engine had already relinquished the descriptor —
	// dropped it from its event loop, live set and conn table, with no
	// OnDisconnect — and the target then refused it
	// ([TransplantTarget.AdoptConn] returned an error). Before celeris#624
	// every one of these closed the descriptor outright: the live gauge had
	// already fallen, no hook fired, and no counter moved, which is exactly
	// the unattributable step this issue chased. The source now re-adopts
	// the connection onto itself instead, so a nonzero value here is paired
	// with a matching TransplantAdopted on the SAME engine and the residual
	// stays zero.
	TransplantHandoffRefused uint64
	// TransplantDrainStopped is the cumulative number of deferred hand-offs
	// that found the drain already stopped ([Engine] reverted) between the
	// detach and the hand-off. The connection is healthy and at a clean
	// boundary, so it is re-adopted onto the source rather than closed —
	// the same silent-close branch as TransplantHandoffRefused, reached by
	// a different route, and counted apart because the fix for a revert
	// race is not the fix for a target that cannot take the descriptor.
	TransplantDrainStopped uint64
	// TransplantStranded is the cumulative number of transplant-pending
	// connections the source's detach-queue drain dropped without handing
	// off, closing, or firing a hook — the already-closed check that runs
	// ahead of the transplant branch. It must stay 0: the two flags are
	// mutually exclusive by construction (a close on an already-detached
	// descriptor returns before it can set the closed flag), and this
	// counter is what makes that argument checkable rather than asserted.
	TransplantStranded uint64
	// TransplantAdoptRefused is the cumulative number of adoptions the
	// TARGET refused for a reason other than an occupied slot — a
	// descriptor outside its conn table, or an event-loop registration
	// that failed. The target owns the descriptor by then, so it closes
	// it AND fires OnDisconnect, keeping accepted - closed - active
	// balanced. Distinct from TransplantAdoptSlotOccupied, which cannot
	// close (the slot holder may close the same number later) and so
	// leaks by design.
	TransplantAdoptRefused uint64
}

// FillErrorClasses copies one engine's per-cause error tally into m and
// derives ErrorCount from it. It is the single wiring point between
// [errclass.Snapshot] and the Error* fields of EngineMetrics, so a bucket
// added to the former has exactly one place to be forgotten in — and
// TestFillErrorClassesSetsEveryBucket fails when it is.
//
// ErrorCount is assigned here, from the buckets, and nowhere else. No engine
// keeps a separate running total that could drift from its parts.
//
// The errclass argument type is internal, so this is reachable only from
// within engine/... — the sub-engines that own the counters.
func FillErrorClasses(m *EngineMetrics, s errclass.Snapshot) {
	m.ErrorCount = s.Total()
	m.ErrorAcceptFDLimit = s.AcceptFDLimit
	m.ErrorAcceptCancelled = s.AcceptCancelled
	m.ErrorAcceptOther = s.AcceptOther
	m.ErrorConnTableCap = s.ConnTableCap
	m.ErrorConnRegister = s.ConnRegister
	m.ErrorListenerRecreate = s.ListenerRecreate
	m.ErrorTransplantAdopt = s.TransplantAdopt
	m.ErrorSendPeerGone = s.SendPeerGone
	m.ErrorSend = s.Send
	m.ErrorRequestBody = s.RequestBody
	m.ErrorHandler = s.Handler
}
