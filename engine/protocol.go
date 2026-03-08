package engine

// Protocol represents the HTTP protocol version.
type Protocol uint8

const (
	HTTP1 Protocol = iota
	H2C
	Auto
)

func (p Protocol) String() string {
	switch p {
	case HTTP1:
		return "http1"
	case H2C:
		return "h2c"
	case Auto:
		return "auto"
	default:
		return "unknown"
	}
}
