package validation

// Counters is the JSON-serializable shape returned by Snapshot.
//
// Defined unconditionally so external callers (probatorium's
// validator-checker, callers in observe.Snapshot) can reference the
// type regardless of build tag. In a production build the values are
// always zero; in a validation build the values are loaded from the
// per-counter atomics in assertions.go.
type Counters struct {
	PanicCount               uint64 `json:"panic_count"`
	RaceFires                uint64 `json:"race_fires"`
	RatelimitTokenViolations uint64 `json:"ratelimit_token_violations"`
	SessionOwnerMismatches   uint64 `json:"session_owner_mismatches"`
	JWTLateAdmits            uint64 `json:"jwt_late_admits"`
	IouringSQECorruptions    uint64 `json:"iouring_sqe_corruptions"`
	AdaptiveSwitchFDLeaks    uint64 `json:"adaptive_switch_fd_leaks"`
}

// SocketPath is the hard-coded unix-socket path the validation
// endpoint listens on. Not configurable on purpose: probatorium's
// validator-checker compiles in the same constant so misconfiguration
// cannot silently disable the cross-process predicate feed.
const SocketPath = "/tmp/celeris-validation.sock"
