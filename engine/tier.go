package engine

// Tier represents an io_uring capability tier.
// Ordering forms a strict hierarchy suitable for >= comparisons.
type Tier uint8

const (
	None     Tier = iota
	Base
	Mid
	High
	Optional
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
