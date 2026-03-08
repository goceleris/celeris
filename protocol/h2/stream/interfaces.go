package stream

import (
	"context"

	"golang.org/x/net/http2"
)

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
