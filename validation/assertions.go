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

// RaceFires is reserved for future use by sites that catch concurrent
// access bugs at runtime (e.g. atomic.CompareAndSwap mismatches that
// indicate a missing lock).
var RaceFires Counter

// RatelimitTokenViolations counts token-bucket invariant breaches
// observed at the allow/undo sites in middleware/ratelimit: token
// count outside [0, capacity], or undo restoring above capacity.
var RatelimitTokenViolations Counter

// SessionOwnerMismatches counts cases where the session admitted on
// the request did not carry the owner that the validation harness
// asserted (e.g. session id reused across logical users).
var SessionOwnerMismatches Counter

// JWTLateAdmits counts JWTs that the middleware admitted with an
// effective exp claim earlier than the wall-clock time at admission.
var JWTLateAdmits Counter

// IouringSQECorruptions counts SQE write-site violations: non-monotonic
// write index, or CQE user_data references that don't resolve to a
// live conn.
var IouringSQECorruptions Counter

// AdaptiveSwitchFDLeaks counts cases where the post-switch FD set is
// not a superset of the pre-switch set minus FDs closed during the
// switch — i.e. a connection was orphaned across the engine swap.
var AdaptiveSwitchFDLeaks Counter

// Snapshot returns a value-typed copy of the counters at the moment
// of the call. Each Load is independent so the snapshot is not a
// consistent slice of a single instant, but counters monotonically
// increase, so a stale read can only undercount — never overcount.
func Snapshot() Counters {
	return Counters{
		PanicCount:               PanicCount.Load(),
		RaceFires:                RaceFires.Load(),
		RatelimitTokenViolations: RatelimitTokenViolations.Load(),
		SessionOwnerMismatches:   SessionOwnerMismatches.Load(),
		JWTLateAdmits:            JWTLateAdmits.Load(),
		IouringSQECorruptions:    IouringSQECorruptions.Load(),
		AdaptiveSwitchFDLeaks:    AdaptiveSwitchFDLeaks.Load(),
	}
}

// Enabled reports whether this build has the validation tag enabled.
// Always true under //go:build validation; the stub in disabled.go
// returns false. Callers use this to gate test fixtures that should
// only run under validation builds.
func Enabled() bool { return true }
