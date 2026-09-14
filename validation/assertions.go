//go:build validation

package validation

import "sync/atomic"

// Counter wraps an atomic.Uint64 with a return-value-free Add so call
// sites are `validation.X.Add(1)` without static-check tools (which
// see the no-op stub in disabled.go) flagging an ignored return.
type Counter struct{ v atomic.Uint64 }

// Add atomically increments the counter by n. The discarded return
// value of the underlying atomic operation is intentional — see the
// type doc.
func (c *Counter) Add(n uint64) { _ = c.v.Add(n) }

// Load returns the current counter value.
func (c *Counter) Load() uint64 { return c.v.Load() }

// Store atomically writes the counter value.
func (c *Counter) Store(n uint64) { c.v.Store(n) }

// PanicCount tracks recovered panics observed by the safety net in
// celeris.routerAdapter.recoverAndRelease and by middleware/recovery.
var PanicCount Counter

// RatelimitTokenViolations counts token-bucket invariant breaches
// observed at the allow/undo sites in middleware/ratelimit: token
// count outside [0, capacity], or undo restoring above capacity.
var RatelimitTokenViolations Counter

// SessionOwnerMismatches counts cases where the session admitted on
// the request did not carry the owner that the validation harness
// asserted (e.g. session id reused across logical users).
var SessionOwnerMismatches Counter

// SessionCookieDrops counts requests on which the session middleware
// could not emit a Set-Cookie (or session-id header) that would have
// CHANGED what the client holds — a fresh or regenerated session id, or
// the clearing cookie — because the handler had already written the
// response body. The client never learns the new id on such a request;
// the persisted session is orphaned until it idles out. A late mutation
// of a loaded session (the client already holds that id) is NOT counted:
// only the cookie's Max-Age refresh is lost, nothing is orphaned.
var SessionCookieDrops Counter

// JWTLateAdmits counts JWTs that the middleware admitted with an
// effective exp claim earlier than the wall-clock time at admission.
var JWTLateAdmits Counter

// IouringSQECorruptions counts SQE write-site violations: non-monotonic
// write index, or CQE user_data references that don't resolve to a
// live conn.
var IouringSQECorruptions Counter

// IouringSendZCSubmits counts SEND_ZC SQEs the io_uring worker armed
// (the zero-copy arm of prepSendSQE). It is the exposure witness the
// SEND_ZC fabric A/B (celeris#585) and the ZC race tier (celeris#587)
// need: without it a clean run cannot be told apart from a run in
// which the ZC branch never executed. Bumped only inside the ZC arm,
// so the plain-SEND per-request hot path is untouched.
var IouringSendZCSubmits Counter

// IouringSendZCSubmitsDetached is the subset of IouringSendZCSubmits
// whose connection had already been handed to a detached middleware
// goroutine (cs.h1State.Detached) — WebSocket / SSE egress. Those are
// the sends that race the inline unix.Write fast path, which is the
// interleaving celeris#587 exercises; a run with submits but zero
// detached submits never put a ZC send and a dispatch-goroutine write
// on the same connection.
var IouringSendZCSubmitsDetached Counter

// IouringSendZCNotifs counts SEND_ZC notification CQEs (CQE_F_NOTIF)
// processed by the worker — the point at which the kernel releases the
// pinned send buffer. A submit without a matching notif is a buffer
// still pinned in DMA, so the submits/notifs pair bounds how long the
// ZC completion cycle stayed open.
var IouringSendZCNotifs Counter

// IouringInlineGuardBlockedZC counts times the detached inline-egress
// fast path declined to issue its raw unix.Write because a SEND_ZC
// notification was still outstanding on that connection
// (cs.zcNotifPending). It is the witness that the guard which keeps a
// dispatch-goroutine write from interleaving with a kernel-held ZC
// buffer actually fired; zero means the ZC-vs-inline window was never
// entered by the load.
var IouringInlineGuardBlockedZC Counter

// IouringZCCompletionWithPendingWrite counts SEND_ZC notification CQEs
// that landed while the connection still had queued bytes in
// cs.writeBuf. That is the state in which the notification hands the
// buffer back and the very next inline write is admitted against data
// the worker has not yet flushed — the ordering celeris#587 checks.
var IouringZCCompletionWithPendingWrite Counter

// Snapshot returns a value-typed copy of the counters at the moment
// of the call. Each Load is independent so the snapshot is not a
// consistent slice of a single instant, but counters monotonically
// increase, so a stale read can only undercount — never overcount.
func Snapshot() Counters {
	return Counters{
		PanicCount:               PanicCount.Load(),
		RatelimitTokenViolations: RatelimitTokenViolations.Load(),
		SessionOwnerMismatches:   SessionOwnerMismatches.Load(),
		SessionCookieDrops:       SessionCookieDrops.Load(),
		JWTLateAdmits:            JWTLateAdmits.Load(),
		IouringSQECorruptions:    IouringSQECorruptions.Load(),

		IouringSendZCSubmits:                IouringSendZCSubmits.Load(),
		IouringSendZCSubmitsDetached:        IouringSendZCSubmitsDetached.Load(),
		IouringSendZCNotifs:                 IouringSendZCNotifs.Load(),
		IouringInlineGuardBlockedZC:         IouringInlineGuardBlockedZC.Load(),
		IouringZCCompletionWithPendingWrite: IouringZCCompletionWithPendingWrite.Load(),
	}
}
