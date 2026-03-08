package engine

// Tier represents an io_uring capability tier.
// Ordering forms a strict hierarchy suitable for >= comparisons.
type Tier uint8

// io_uring capability tiers in ascending order of feature availability.
const (
	None     Tier = iota // no io_uring support
	Base                 // kernel 5.10+
	Mid                  // kernel 5.13+ (COOP_TASKRUN)
	High                 // kernel 5.19+ (multishot accept/recv, provided buffers)
	Optional             // kernel 6.0+ (SINGLE_ISSUER, SQPOLL)
)

func (t Tier) String() string {
	switch t {
	case None:
		return "none"
	case Base:
		return "base"
	case Mid:
		return "mid"
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
