package websocket

// Opcode represents a WebSocket frame opcode (RFC 6455 Section 5.2).
type Opcode byte

const (
	// OpContinuation is the continuation frame opcode.
	OpContinuation Opcode = 0x0
	// OpText is the text data frame opcode.
	OpText Opcode = 0x1
	// OpBinary is the binary data frame opcode.
	OpBinary Opcode = 0x2

	// OpClose is the connection close control frame opcode.
	OpClose Opcode = 0x8
	// OpPing is the ping control frame opcode.
	OpPing Opcode = 0x9
	// OpPong is the pong control frame opcode.
	OpPong Opcode = 0xA
)

// IsControl returns true for close, ping, and pong frames.
func (o Opcode) IsControl() bool { return o >= 0x8 }

// IsData returns true for text, binary, and continuation frames.
func (o Opcode) IsData() bool { return o < 0x8 }

// Close status codes (RFC 6455 Section 7.4.1).
const (
	CloseNormalClosure    = 1000
	CloseGoingAway        = 1001
	CloseProtocolError    = 1002
	CloseUnsupportedData  = 1003
	CloseNoStatusReceived = 1005
	CloseAbnormalClosure  = 1006
	CloseInvalidPayload   = 1007
	ClosePolicyViolation  = 1008
	CloseMessageTooBig    = 1009
	CloseMandatoryExt     = 1010
	CloseInternalError    = 1011
	CloseServiceRestart   = 1012
	CloseTryAgainLater    = 1013
)
