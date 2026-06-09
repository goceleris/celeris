package engine

// Tier represents an io_uring capability tier.
// Ordering forms a strict hierarchy suitable for >= comparisons.
type Tier uint8

// io_uring capability tiers in ascending order of feature availability.
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
