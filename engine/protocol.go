package engine

// Protocol represents the HTTP protocol version.
type Protocol uint8

// HTTP protocol version constants. The zero value is protocolDefault which
// resolves to Auto (see resource.Config.WithDefaults).
const (
	protocolDefault Protocol = iota // resolved at config time
	HTTP1                           // HTTP/1.1
	H2C                             // HTTP/2 cleartext
	Auto                            // auto-negotiate
)

// IsDefault reports whether the protocol is the unset zero value.
func (p Protocol) IsDefault() bool { return p == protocolDefault }

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
