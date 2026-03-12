package stream

import (
	"context"
	"errors"
	"net"

	"golang.org/x/net/http2"
)

// ErrHijackNotSupported is returned when the engine does not support
// connection takeover.
var ErrHijackNotSupported = errors.New("celeris: hijack not supported by this engine")

// Hijacker is implemented by ResponseWriters that support connection takeover.
// This enables protocols like WebSocket that require raw TCP access.
type Hijacker interface {
	Hijack(stream *Stream) (net.Conn, error)
}

// Handler interface for processing streams.
type Handler interface {
	HandleStream(ctx context.Context, stream *Stream) error
}

// HandlerFunc is an adapter to use functions as stream handlers.
type HandlerFunc func(ctx context.Context, stream *Stream) error

// HandleStream calls the handler function.
func (f HandlerFunc) HandleStream(ctx context.Context, stream *Stream) error {
	return f(ctx, stream)
}

// FrameWriter is an interface for writing HTTP/2 frames.
type FrameWriter interface {
	WriteSettings(settings ...http2.Setting) error
	WriteSettingsAck() error
	WriteHeaders(streamID uint32, endStream bool, headerBlock []byte, maxFrameSize uint32) error
	WriteData(streamID uint32, endStream bool, data []byte) error
	WriteWindowUpdate(streamID uint32, increment uint32) error
	WriteRSTStream(streamID uint32, code http2.ErrCode) error
	WriteGoAway(lastStreamID uint32, code http2.ErrCode, debugData []byte) error
	WritePing(ack bool, data [8]byte) error
	WritePushPromise(streamID uint32, promiseID uint32, endHeaders bool, headerBlock []byte) error
}

// ResponseWriter is an interface for writing responses.
type ResponseWriter interface {
	WriteResponse(stream *Stream, status int, headers [][2]string, body []byte) error
	SendGoAway(lastStreamID uint32, code http2.ErrCode, debug []byte) error
	MarkStreamClosed(streamID uint32)
	IsStreamClosed(streamID uint32) bool
	WriteRSTStreamPriority(streamID uint32, code http2.ErrCode) error
	CloseConn() error
}

// Streamer supports incremental response writing. Engines that support
// streaming implement this interface on their ResponseWriter. The existing
// WriteResponse path is preserved for non-streaming responses (hot path).
type Streamer interface {
	// WriteHeader sends the status line and headers. Must be called once before Write.
	WriteHeader(stream *Stream, status int, headers [][2]string) error
	// Write sends a chunk of the response body. May be called multiple times.
	Write(stream *Stream, data []byte) error
	// Flush ensures buffered data is sent to the network.
	Flush(stream *Stream) error
	// Close signals end of the response body.
	Close(stream *Stream) error
}
