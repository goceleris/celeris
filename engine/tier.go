package engine

// Tier represents an io_uring capability tier.
// Ordering forms a strict hierarchy suitable for >= comparisons.
type Tier uint8

// io_uring capability tiers in ascending order of feature availability.
//
// The previous `Mid` tier (kernel 5.13–5.18) was retired in celeris
// v1.4.8 because its only distinguishing feature (IORING_SETUP_COOP_TASKRUN)
// was actually introduced in kernel 5.19, not 5.13 — pre-v1.4.8 every
// 5.13–5.18 kernel (Ubuntu 22.04 LTS / 5.15 included) attempted
// io_uring_setup with a flag the kernel rejects with -EINVAL, forcing a
// noisy fall-back to Base. With the gate corrected, the 5.13–5.18 range
// gains no capability over 5.10–5.12 and collapses into Base. See
// celeris#287.
const (
	None     Tier = iota // no io_uring support
	Base                 // kernel 5.10+ (LTS-stable io_uring baseline; linked SQE chains, single-shot accept/recv)
	High                 // kernel 5.19+ (multishot accept/recv, provided buffers, fixed files, COOP_TASKRUN, SINGLE_ISSUER)
	Optional             // kernel 6.0+ (adds SQPOLL, SEND_ZC; 6.1+ swaps COOP_TASKRUN → DEFER_TASKRUN)
)

// String returns the tier name (e.g. "none", "base", "high").
func (t Tier) String() string {
	switch t {
	case None:
		return "none"
	case Base:
		return "base"
	case High:
		return "high"
	case Optional:
		return "optional"
	default:
		return "unknown"
	}
}

// Available reports whether this tier represents a detected capability.
func (t Tier) Available() bool {
	return t > None
}
