package compress

import (
	"io"
	"sync"

	"github.com/andybalholm/brotli"
	kgzip "github.com/klauspost/compress/gzip"

	"github.com/goceleris/celeris"
)

var (
	streamGzipPool = sync.Pool{
		New: func() any {
			w, _ := kgzip.NewWriterLevel(nil, kgzip.DefaultCompression)
			return w
		},
	}
	streamBrotliPool = sync.Pool{
		New: func() any {
			return brotli.NewWriterLevel(nil, int(defaultBrotliLevel))
		},
	}
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
	pool     *sync.Pool
}

// NewCompressedStream wraps sw with compression for the given encoding.
// Supported encodings: "gzip", "br". Returns nil if the encoding is
// unsupported or sw is nil.
//
// Sets Content-Encoding and Vary headers automatically on [CompressedStream.WriteHeader].
func NewCompressedStream(sw *celeris.StreamWriter, encoding string) *CompressedStream {
	if sw == nil {
		return nil
	}
	cs := &CompressedStream{sw: sw, encoding: encoding}
	switch encoding {
	case "gzip":
		cs.pool = &streamGzipPool
		w := streamGzipPool.Get().(*kgzip.Writer)
		w.Reset(writerFunc(cs.writeRaw))
		cs.writer = w
	case "br":
		cs.pool = &streamBrotliPool
		w := streamBrotliPool.Get().(*brotli.Writer)
		w.Reset(writerFunc(cs.writeRaw))
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
// Vary added automatically. Must be called once before Write.
func (cs *CompressedStream) WriteHeader(status int, headers [][2]string) error {
	extra := make([][2]string, 0, len(headers)+2)
	extra = append(extra, [2]string{"content-encoding", cs.encoding})
	extra = append(extra, [2]string{"vary", "Accept-Encoding"})
	extra = append(extra, headers...)
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
	// brotli.Writer has Flush method.
	case interface{ Flush() error }:
		if err := w.Flush(); err != nil {
			return err
		}
	}
	return cs.sw.Flush()
}

// Close closes the compressor (writes final bytes/trailer) and the underlying
// StreamWriter. The compressor is returned to its pool after close.
func (cs *CompressedStream) Close() error {
	err := cs.writer.Close()
	if cs.pool != nil {
		cs.pool.Put(cs.writer)
	}
	if cerr := cs.sw.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}
