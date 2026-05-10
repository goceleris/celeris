//go:build !validation

package validation

// counter is the no-op stub installed in production builds. It carries
// no state, exposes the same Add/Load/Store surface as atomic.Uint64,
// and inlines to nothing. Importers (engine/iouring, middleware/...)
// can therefore call validation.X.Add(1) without a build-tag guard at
// the call site — production simply discards the increment.
type counter struct{}

func (counter) Add(uint64) uint64 { return 0 }
func (counter) Load() uint64      { return 0 }
func (counter) Store(uint64)      {}

// PanicCount and friends are the stub instances. Same identifier
// surface as the atomic.Uint64 vars in assertions.go so call sites
// compile identically in both modes.
var (
	PanicCount               counter
	RaceFires                counter
	RatelimitTokenViolations counter
	SessionOwnerMismatches   counter
	JWTLateAdmits            counter
	IouringSQECorruptions    counter
	AdaptiveSwitchFDLeaks    counter
)

// Snapshot returns the zero value in production builds — no counters
// exist and no socket is served. Kept defined so observe.Snapshot can
// embed Counters unconditionally without a build-tag split.
func Snapshot() Counters { return Counters{} }

// Enabled reports whether this build has the validation tag enabled.
// Always false in production.
func Enabled() bool { return false }
