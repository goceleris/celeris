package compress

import (
	"io"

	"github.com/andybalholm/brotli"
	kgzip "github.com/klauspost/compress/gzip"

	"github.com/goceleris/celeris"
)

// CompressedStream wraps a [celeris.StreamWriter] with on-the-fly compression.
// The caller obtains a StreamWriter from [celeris.Context.StreamWriter], then
// wraps it with NewCompressedStream for transparent compression.
//
// Supported encodings: "gzip" and "br" (brotli). Zstd uses EncodeAll (batch)
// and is not suitable for streaming; use the buffered middleware ([New]) for zstd.
//
// Unlike the buffered compress middleware, CompressedStream does not support
// the expansion guard or MinLength threshold — the response is compressed
// unconditionally.
type CompressedStream struct {
	sw       *celeris.StreamWriter
	writer   io.WriteCloser
	encoding string
	closed   bool
}

// StreamOption configures streaming compression.
type StreamOption func(*streamConfig)

type streamConfig struct {
	gzipLevel   int
	brotliLevel int
}

// WithGzipLevel sets the gzip compression level for streaming.
// Default: gzip.DefaultCompression (-1).
func WithGzipLevel(level Level) StreamOption {
	return func(c *streamConfig) { c.gzipLevel = resolveGzipLevel(level) }
}

// WithBrotliLevel sets the brotli compression level for streaming.
// Default: 6.
func WithBrotliLevel(level Level) StreamOption {
	return func(c *streamConfig) { c.brotliLevel = resolveBrotliLevel(level) }
}

// NewCompressedStream wraps sw with compression for the given encoding.
// Supported encodings: "gzip", "br". Returns nil if the encoding is
// unsupported or sw is nil.
//
// Optional [StreamOption] values configure compression levels. Without
// options, library defaults are used (gzip level 6, brotli level 6).
//
// Content-Encoding and Vary headers are set automatically on
// [CompressedStream.WriteHeader]. Any Content-Length in the user headers
// is stripped (streaming responses use chunked transfer encoding).
func NewCompressedStream(sw *celeris.StreamWriter, encoding string, opts ...StreamOption) *CompressedStream {
	if sw == nil {
		return nil
	}

	cfg := streamConfig{
		gzipLevel:   kgzip.DefaultCompression,
		brotliLevel: int(defaultBrotliLevel),
	}
	for _, o := range opts {
		o(&cfg)
	}

	cs := &CompressedStream{sw: sw, encoding: encoding}
	switch encoding {
	case "gzip":
		w, err := kgzip.NewWriterLevel(writerFunc(cs.writeRaw), cfg.gzipLevel)
		if err != nil {
			return nil
		}
		cs.writer = w
	case "br":
		w := brotli.NewWriterLevel(writerFunc(cs.writeRaw), cfg.brotliLevel)
		cs.writer = w
	default:
		return nil
	}
	return cs
}

// writerFunc adapts a function to the io.Writer interface. The gzip/brotli
// writers call Write on their destination; this bridges them to StreamWriter.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// writeRaw sends compressed bytes to the underlying StreamWriter.
func (cs *CompressedStream) writeRaw(p []byte) (int, error) {
	return cs.sw.Write(p)
}

// WriteHeader sends the status line and headers with Content-Encoding and
// Vary added automatically. Any Content-Length header is stripped because
// streaming responses use chunked transfer encoding. Must be called once
// before Write.
func (cs *CompressedStream) WriteHeader(status int, headers [][2]string) error {
	extra := make([][2]string, 0, len(headers)+2)
	extra = append(extra, [2]string{"content-encoding", cs.encoding})
	extra = append(extra, [2]string{"vary", "Accept-Encoding"})
	for _, h := range headers {
		if h[0] != "content-length" {
			extra = append(extra, h)
		}
	}
	return cs.sw.WriteHeader(status, extra)
}

// Write compresses data and sends it to the underlying StreamWriter.
func (cs *CompressedStream) Write(data []byte) (int, error) {
	return cs.writer.Write(data)
}

// Flush flushes the compressor and the underlying StreamWriter.
func (cs *CompressedStream) Flush() error {
	switch w := cs.writer.(type) {
	case *kgzip.Writer:
		if err := w.Flush(); err != nil {
			return err
		}
	case interface{ Flush() error }:
		if err := w.Flush(); err != nil {
			return err
		}
	}
	return cs.sw.Flush()
}

// Close closes the compressor (writes final bytes/trailer) and the underlying
// StreamWriter. Safe to call multiple times; subsequent calls are no-ops.
func (cs *CompressedStream) Close() error {
	if cs.closed {
		return nil
	}
	cs.closed = true
	err := cs.writer.Close()
	if cs.sw != nil {
		if cerr := cs.sw.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}
