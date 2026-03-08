package frame

import (
	"bytes"
	"fmt"
	"sync"

	"golang.org/x/net/http2/hpack"
)

// headerBufPool reuses temporary buffers used during HPACK encoding to reduce allocations.
var headerBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

// HeaderEncoder encodes HTTP headers using HPACK.
type HeaderEncoder struct {
	mu      sync.Mutex
	encoder *hpack.Encoder
	buf     *bytes.Buffer
}

// NewHeaderEncoder creates a new header encoder.
func NewHeaderEncoder() *HeaderEncoder {
	bufAny := headerBufPool.Get()
	var buf *bytes.Buffer
	if b, ok := bufAny.(*bytes.Buffer); ok {
		b.Reset()
		buf = b
	} else {
		buf = new(bytes.Buffer)
	}
	return &HeaderEncoder{
		encoder: hpack.NewEncoder(buf),
		buf:     buf,
	}
}

// Encode encodes headers to HPACK format. Returns a copy safe for concurrent use.
func (e *HeaderEncoder) Encode(headers [][2]string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.buf.Reset()
	for _, h := range headers {
		if err := e.encoder.WriteField(hpack.HeaderField{Name: h[0], Value: h[1]}); err != nil {
			return nil, err
		}
	}
	result := make([]byte, e.buf.Len())
	copy(result, e.buf.Bytes())
	return result, nil
}

// EncodeBorrow encodes headers and returns a byte slice backed by the encoder's
// internal buffer. The returned slice is only valid until the next call to
// Encode/EncodeBorrow or Close.
func (e *HeaderEncoder) EncodeBorrow(headers [][2]string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.buf.Reset()
	for _, h := range headers {
		if err := e.encoder.WriteField(hpack.HeaderField{Name: h[0], Value: h[1]}); err != nil {
			return nil, err
		}
	}
	return e.buf.Bytes(), nil
}

// Close releases internal resources back to the pool.
func (e *HeaderEncoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.buf != nil {
		e.buf.Reset()
		headerBufPool.Put(e.buf)
		e.buf = nil
		e.encoder = hpack.NewEncoder(new(bytes.Buffer))
	}
}

// HeaderDecoder decodes HTTP headers using HPACK.
type HeaderDecoder struct {
	decoder *hpack.Decoder
}

// NewHeaderDecoder creates a new header decoder.
func NewHeaderDecoder(maxSize uint32) *HeaderDecoder {
	return &HeaderDecoder{
		decoder: hpack.NewDecoder(maxSize, nil),
	}
}

// Decode decodes HPACK-encoded headers.
func (d *HeaderDecoder) Decode(data []byte) ([][2]string, error) {
	headers := make([][2]string, 0)
	d.decoder.SetEmitFunc(func(hf hpack.HeaderField) {
		headers = append(headers, [2]string{hf.Name, hf.Value})
	})

	if _, err := d.decoder.Write(data); err != nil {
		return nil, fmt.Errorf("hpack decode error: %w", err)
	}

	return headers, nil
}
