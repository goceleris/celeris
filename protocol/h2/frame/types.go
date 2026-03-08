package frame

// Type represents HTTP/2 frame types.
type Type uint8

const (
	FrameData         Type = 0x0
	FrameHeaders      Type = 0x1
	FramePriority     Type = 0x2
	FrameRSTStream    Type = 0x3
	FrameSettings     Type = 0x4
	FramePushPromise  Type = 0x5
	FramePing         Type = 0x6
	FrameGoAway       Type = 0x7
	FrameWindowUpdate Type = 0x8
	FrameContinuation Type = 0x9
)

var frameTypeNames = [10]string{
	"DATA",
	"HEADERS",
	"PRIORITY",
	"RST_STREAM",
	"SETTINGS",
	"PUSH_PROMISE",
	"PING",
	"GOAWAY",
	"WINDOW_UPDATE",
	"CONTINUATION",
}

func (t Type) String() string {
	if int(t) < len(frameTypeNames) {
		return frameTypeNames[t]
	}
	return "UNKNOWN"
}

// Flags represents HTTP/2 frame flags.
type Flags uint8

const (
	FlagAck        Flags = 0x1
	FlagEndStream  Flags = 0x1
	FlagEndHeaders Flags = 0x4
	FlagPadded     Flags = 0x8
	FlagPriority   Flags = 0x20
)

// Frame represents a generic HTTP/2 frame.
type Frame struct {
	Type     Type
	Flags    Flags
	StreamID uint32
	Payload  []byte
}
