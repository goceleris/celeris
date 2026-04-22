package frame

import (
	"io"
	"sync"

	"golang.org/x/net/http2"
)

// Writer handles HTTP/2 frame writing with mutex protection.
type Writer struct {
	framer       *http2.Framer
	writer       io.Writer
	mu           sync.Mutex
	maxFrameSize uint32 // peer's SETTINGS_MAX_FRAME_SIZE; 0 = use RFC default 16 KiB
}

// SetMaxFrameSize updates the peer's advertised SETTINGS_MAX_FRAME_SIZE.
// Subsequent WriteData calls will split payloads larger than this value into
// multiple DATA frames. Safe to call from any goroutine.
func (w *Writer) SetMaxFrameSize(v uint32) {
	w.mu.Lock()
	w.maxFrameSize = v
	w.mu.Unlock()
}

// NewWriter creates a new frame writer. The underlying http2.Framer is
// created lazily on first non-trivial call (WriteFrame, WriteHeaders,
// WriteData, WriteGoAway, WriteRawFrame, etc). Fixed-shape control frames
// (WriteSettingsAck) are emitted directly without constructing the Framer
// at all — useful for h2c handshakes that close before any HPACK-requiring
// frame is sent.
func NewWriter(w io.Writer) *Writer {
	return &Writer{writer: w}
}

// ensureFramer lazily constructs the underlying http2.Framer. Callers must
// hold w.mu.
func (w *Writer) ensureFramer() {
	if w.framer == nil {
		w.framer = http2.NewFramer(w.writer, nil)
	}
}

// Flush flushes any buffered data.
func (w *Writer) Flush() error {
	if flusher, ok := w.writer.(interface{ Flush() error }); ok {
		return flusher.Flush()
	}
	return nil
}

// Close closes the underlying writer if it implements Close.
func (w *Writer) Close() error {
	if closer, ok := w.writer.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// CloseImmediate immediately closes the underlying writer.
func (w *Writer) CloseImmediate() error {
	if closer, ok := w.writer.(interface{ CloseImmediate() error }); ok {
		return closer.CloseImmediate()
	}
	if closer, ok := w.writer.(interface{ Close() error }); ok {
		return closer.Close()
	}
	return nil
}

// WriteFrame writes a generic frame.
func (w *Writer) WriteFrame(f *Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ensureFramer()
	flags := http2.Flags(f.Flags)
	return w.framer.WriteRawFrame(http2.FrameType(f.Type), flags, f.StreamID, f.Payload)
}

// WriteSettings writes a SETTINGS frame.
func (w *Writer) WriteSettings(settings ...http2.Setting) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ensureFramer()
	return w.framer.WriteSettings(settings...)
}

// settingsAckFrame is the exact 9-byte wire encoding of an H2 SETTINGS ACK
// frame (length=0, type=SETTINGS (4), flags=ACK (1), streamID=0). Written
// directly rather than through http2.Framer — the Framer validates frame
// sizes and reuses internal buffers, neither of which add any value for
// this fixed-shape frame.
var settingsAckFrame = [9]byte{0, 0, 0, 0x04, 0x01, 0, 0, 0, 0}

// WriteSettingsAck writes a SETTINGS acknowledgment frame.
func (w *Writer) WriteSettingsAck() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := w.writer.Write(settingsAckFrame[:])
	return err
}

// WriteHeaders writes HEADERS (and CONTINUATION) frames, fragmenting by maxFrameSize.
func (w *Writer) WriteHeaders(streamID uint32, endStream bool, headerBlock []byte, maxFrameSize uint32) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ensureFramer()

	if maxFrameSize == 0 {
		maxFrameSize = 16384
	}

	remaining := headerBlock
	first := true
	for len(remaining) > 0 {
		chunkLen := int(maxFrameSize)
		if len(remaining) < chunkLen {
			chunkLen = len(remaining)
		}
		frag := remaining[:chunkLen]
		remaining = remaining[chunkLen:]

		if first {
			var flags http2.Flags
			if endStream {
				flags |= http2.FlagHeadersEndStream
			}
			if len(remaining) == 0 {
				flags |= http2.FlagHeadersEndHeaders
			}
			if err := w.framer.WriteRawFrame(http2.FrameHeaders, flags, streamID, frag); err != nil {
				return err
			}
			first = false
		} else {
			var flags http2.Flags
			if len(remaining) == 0 {
				flags |= http2.FlagContinuationEndHeaders
			}
			if err := w.framer.WriteRawFrame(http2.FrameContinuation, flags, streamID, frag); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteData writes one or more DATA frames, fragmenting by the peer's
// advertised SETTINGS_MAX_FRAME_SIZE (RFC 7540 §4.2). When
// SetMaxFrameSize has not been called the writer defaults to 16 KiB.
// endStream is applied to the final fragment only.
func (w *Writer) WriteData(streamID uint32, endStream bool, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(data) == 0 && !endStream {
		return nil
	}
	w.ensureFramer()

	maxFrame := int(w.maxFrameSize)
	if maxFrame <= 0 {
		maxFrame = 16384
	}

	// Fast path: fits in a single frame.
	if len(data) <= maxFrame {
		return w.framer.WriteData(streamID, endStream, data)
	}

	remaining := data
	for len(remaining) > 0 {
		chunkLen := maxFrame
		if len(remaining) < chunkLen {
			chunkLen = len(remaining)
		}
		isLast := chunkLen == len(remaining)
		if err := w.framer.WriteData(streamID, isLast && endStream, remaining[:chunkLen]); err != nil {
			return err
		}
		remaining = remaining[chunkLen:]
	}
	return nil
}

// WriteWindowUpdate writes a WINDOW_UPDATE frame.
func (w *Writer) WriteWindowUpdate(streamID uint32, increment uint32) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ensureFramer()
	return w.framer.WriteWindowUpdate(streamID, increment)
}

// WriteRSTStream writes a RST_STREAM frame.
func (w *Writer) WriteRSTStream(streamID uint32, code http2.ErrCode) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ensureFramer()
	return w.framer.WriteRSTStream(streamID, code)
}

// WriteGoAway writes a GOAWAY frame.
func (w *Writer) WriteGoAway(lastStreamID uint32, code http2.ErrCode, debugData []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ensureFramer()
	return w.framer.WriteGoAway(lastStreamID, code, debugData)
}

// WritePing writes a PING frame.
func (w *Writer) WritePing(ack bool, data [8]byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ensureFramer()
	return w.framer.WritePing(ack, data)
}

// WritePushPromise writes a PUSH_PROMISE frame.
func (w *Writer) WritePushPromise(streamID uint32, promiseID uint32, endHeaders bool, headerBlock []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ensureFramer()
	var flags http2.Flags
	if endHeaders {
		flags = http2.FlagPushPromiseEndHeaders
	}
	return w.framer.WriteRawFrame(http2.FramePushPromise, flags, streamID, append(
		[]byte{
			byte(promiseID >> 24),
			byte(promiseID >> 16),
			byte(promiseID >> 8),
			byte(promiseID),
		},
		headerBlock...,
	))
}
