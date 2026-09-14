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
	RatelimitTokenViolations uint64 `json:"ratelimit_token_violations"`
	SessionOwnerMismatches   uint64 `json:"session_owner_mismatches"`
	SessionCookieDrops       uint64 `json:"session_cookie_drops"`
	JWTLateAdmits            uint64 `json:"jwt_late_admits"`
	IouringSQECorruptions    uint64 `json:"iouring_sqe_corruptions"`

	// SEND_ZC exposure witnesses (celeris#591). They are not violation
	// counts: a predicate over the zero-copy send path may only be
	// declared when these show the branch ran at all (celeris#585 fabric
	// A/B, celeris#587 race tier).
	IouringSendZCSubmits                uint64 `json:"iouring_send_zc_submits"`
	IouringSendZCSubmitsDetached        uint64 `json:"iouring_send_zc_submits_detached"`
	IouringSendZCNotifs                 uint64 `json:"iouring_send_zc_notifs"`
	IouringInlineGuardBlockedZC         uint64 `json:"iouring_inline_guard_blocked_zc"`
	IouringZCCompletionWithPendingWrite uint64 `json:"iouring_zc_completion_with_pending_write"`
}
