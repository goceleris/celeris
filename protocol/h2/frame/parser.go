package frame

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"golang.org/x/net/http2"
)

// Parser handles HTTP/2 frame parsing.
type Parser struct {
	framer *http2.Framer
	buf    *bytes.Buffer
}

// NewParser creates a new frame parser.
func NewParser() *Parser {
	buf := new(bytes.Buffer)
	return &Parser{
		framer: nil,
		buf:    buf,
	}
}

// InitReader binds the parser to a persistent reader. This allows the framer
// to preserve CONTINUATION expectations across frames and read progressively as
// more data arrives.
func (p *Parser) InitReader(r io.Reader) {
	p.framer = http2.NewFramer(p.buf, r)
	p.framer.SetMaxReadFrameSize(1 << 20)
}

// ReadNextFrame reads the next frame using the bound reader.
func (p *Parser) ReadNextFrame() (http2.Frame, error) {
	if p.framer == nil {
		return nil, fmt.Errorf("parser not initialized; call InitReader")
	}
	return p.framer.ReadFrame()
}

// RawFrame is a zero-copy frame representation. Payload is a slice into the
// caller's buffer and is only valid until the next buffer modification.
type RawFrame struct {
	Type     byte
	Flags    byte
	StreamID uint32
	Length   uint32
	Payload  []byte
}

// HasEndStream reports whether the END_STREAM flag (0x01) is set.
func (f RawFrame) HasEndStream() bool { return f.Flags&0x01 != 0 }

// HasEndHeaders reports whether the END_HEADERS flag (0x04) is set.
func (f RawFrame) HasEndHeaders() bool { return f.Flags&0x04 != 0 }

// HasPriority reports whether the PRIORITY flag (0x20) is set (HEADERS frames).
func (f RawFrame) HasPriority() bool { return f.Flags&0x20 != 0 }

// HasPadded reports whether the PADDED flag (0x08) is set.
func (f RawFrame) HasPadded() bool { return f.Flags&0x08 != 0 }

// ReadRawFrame parses a raw frame from buf without allocating. Returns the
// frame and the total number of bytes consumed (9 + payload length).
// Returns io.ErrUnexpectedEOF if buf doesn't contain a complete frame.
func ReadRawFrame(buf []byte) (RawFrame, int, error) {
	if len(buf) < 9 {
		return RawFrame{}, 0, io.ErrUnexpectedEOF
	}
	length := uint32(buf[0])<<16 | uint32(buf[1])<<8 | uint32(buf[2])
	total := int(9 + length)
	if len(buf) < total {
		return RawFrame{}, 0, io.ErrUnexpectedEOF
	}
	return RawFrame{
		Type:     buf[3],
		Flags:    buf[4],
		StreamID: binary.BigEndian.Uint32(buf[5:9]) & 0x7fffffff,
		Length:   length,
		Payload:  buf[9:total],
	}, total, nil
}

// Parse reads and parses an HTTP/2 frame from the reader.
func (p *Parser) Parse(r io.Reader) (*Frame, error) {
	p.buf.Reset()

	header := make([]byte, 9)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}

	length := uint32(header[0])<<16 | uint32(header[1])<<8 | uint32(header[2])
	frameType := Type(header[3])
	flags := Flags(header[4])
	streamID := binary.BigEndian.Uint32(header[5:9]) & 0x7fffffff

	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return nil, err
		}
	}

	return &Frame{
		Type:     frameType,
		Flags:    flags,
		StreamID: streamID,
		Payload:  payload,
	}, nil
}
