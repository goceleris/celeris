package engine

// Protocol represents the HTTP protocol version.
type Protocol uint8

// HTTP protocol version constants.
const (
	HTTP1 Protocol = iota // HTTP/1.1
	H2C                   // HTTP/2 cleartext
	Auto                  // auto-negotiate
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
