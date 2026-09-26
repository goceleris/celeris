//go:build !validation

package validation

import "time"

// Counter is the no-op stub installed in production builds. It carries
// no state and inlines to nothing. Importers (engine/iouring,
// middleware/...) call validation.X.Add(1) without a build-tag guard at
// the call site — production simply discards the increment. Add returns
// nothing so static-check tools cannot flag callers for ignoring an
// unused return value.
type Counter struct{}

// Add is the production no-op increment.
func (Counter) Add(uint64) {}

// Load returns zero in production.
func (Counter) Load() uint64 { return 0 }

// Store is the production no-op write.
func (Counter) Store(uint64) {}

// PanicCount and friends are the stub instances. Same identifier
// surface as the wrapped counter vars in assertions.go so call sites
// compile identically in both modes.
var (
	PanicCount               Counter
	RatelimitTokenViolations Counter
	SessionOwnerMismatches   Counter
	SessionCookieDrops       Counter
	JWTLateAdmits            Counter
	IouringSQECorruptions    Counter

	IouringSendZCSubmits                Counter
	IouringSendZCSubmitsDetached        Counter
	IouringSendZCNotifs                 Counter
	IouringInlineGuardBlockedZC         Counter
	IouringZCCompletionWithPendingWrite Counter
	IouringSendZCFallbacks              Counter
)

// Enabled is false in production: code guarded by it compiles away.
const Enabled = false

// SetZCWindowHold is the production no-op: the celeris#587 SEND_ZC
// window hold exists only under -tags=validation (zc_window.go).
func SetZCWindowHold(time.Duration) {}

// ZCWindowHold is the production no-op. Its io_uring call sites are also
// guarded by the false Enabled constant, so they compile away entirely.
func ZCWindowHold() {}

// Snapshot returns the zero value in production builds — no counters
// exist and no socket is served. Kept defined so observe.Snapshot can
// embed Counters unconditionally without a build-tag split.
func Snapshot() Counters { return Counters{} }
