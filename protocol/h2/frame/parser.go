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
