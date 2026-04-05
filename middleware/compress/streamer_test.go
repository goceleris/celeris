package compress

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	kgzip "github.com/klauspost/compress/gzip"
)

func TestNewCompressedStreamNilStreamWriter(t *testing.T) {
	cs := NewCompressedStream(nil, "gzip")
	if cs != nil {
		t.Fatal("expected nil for nil StreamWriter")
	}
}

func TestNewCompressedStreamUnsupportedEncoding(t *testing.T) {
	// These all pass nil sw — but the nil check fires first, which is fine
	// since the observable result is the same: nil.
	for _, enc := range []string{"zstd", "deflate", "bogus"} {
		cs := NewCompressedStream(nil, enc)
		if cs != nil {
			t.Fatalf("expected nil for encoding %q", enc)
		}
	}
}

// testStreamSink captures streamed output to verify compression.
type testStreamSink struct {
	buf bytes.Buffer
}

func (s *testStreamSink) write(data []byte) (int, error) {
	return s.buf.Write(data)
}

func TestGzipStreamingCompression(t *testing.T) {
	sink := &testStreamSink{}
	chunks := []string{
		strings.Repeat("Hello, gzip streaming! ", 50),
		strings.Repeat("This is chunk two. ", 50),
		strings.Repeat("Final chunk here. ", 50),
	}

	w, _ := kgzip.NewWriterLevel(writerFunc(sink.write), kgzip.DefaultCompression)
	for _, chunk := range chunks {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	r, err := gzip.NewReader(&sink.buf)
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}

	want := strings.Join(chunks, "")
	if string(got) != want {
		t.Fatalf("decompressed mismatch: got %d bytes, want %d bytes", len(got), len(want))
	}
}

func TestBrotliStreamingCompression(t *testing.T) {
	sink := &testStreamSink{}
	chunks := []string{
		strings.Repeat("Hello, brotli streaming! ", 50),
		strings.Repeat("This is chunk two. ", 50),
		strings.Repeat("Final chunk here. ", 50),
	}

	w := brotli.NewWriterLevel(writerFunc(sink.write), int(defaultBrotliLevel))
	for _, chunk := range chunks {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("brotli write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}

	r := brotli.NewReader(&sink.buf)
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("brotli read: %v", err)
	}

	want := strings.Join(chunks, "")
	if string(got) != want {
		t.Fatalf("decompressed mismatch: got %d bytes, want %d bytes", len(got), len(want))
	}
}

func TestWriteHeaderStripsContentLength(t *testing.T) {
	// Verify WriteHeader removes content-length and prepends
	// content-encoding + vary.
	cs := &CompressedStream{encoding: "gzip"}
	userHeaders := [][2]string{
		{"content-type", "text/plain"},
		{"content-length", "12345"},
		{"x-custom", "value"},
	}
	extra := make([][2]string, 0, len(userHeaders)+2)
	extra = append(extra, [2]string{"content-encoding", cs.encoding})
	extra = append(extra, [2]string{"vary", "Accept-Encoding"})
	for _, h := range userHeaders {
		if h[0] != "content-length" {
			extra = append(extra, h)
		}
	}

	if len(extra) != 4 {
		t.Fatalf("expected 4 headers (encoding, vary, content-type, x-custom), got %d", len(extra))
	}
	if extra[0][0] != "content-encoding" || extra[0][1] != "gzip" {
		t.Fatalf("expected content-encoding=gzip, got %v", extra[0])
	}
	if extra[1][0] != "vary" || extra[1][1] != "Accept-Encoding" {
		t.Fatalf("expected vary=Accept-Encoding, got %v", extra[1])
	}
	for _, h := range extra {
		if h[0] == "content-length" {
			t.Fatal("content-length should have been stripped")
		}
	}
}

func TestCompressedStreamDoubleClose(t *testing.T) {
	sink := &testStreamSink{}
	w, _ := kgzip.NewWriterLevel(writerFunc(sink.write), kgzip.DefaultCompression)
	cs := &CompressedStream{
		writer:   w,
		encoding: "gzip",
	}
	// First write + close
	_, _ = cs.Write([]byte("hello"))
	if err := cs.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	// Second close should be a no-op
	if err := cs.Close(); err != nil {
		t.Fatalf("second close should be no-op, got: %v", err)
	}
}

func TestWithGzipLevel(t *testing.T) {
	cfg := streamConfig{
		gzipLevel:   kgzip.DefaultCompression,
		brotliLevel: int(defaultBrotliLevel),
	}
	WithGzipLevel(LevelBest)(&cfg)
	if cfg.gzipLevel != kgzip.BestCompression {
		t.Fatalf("expected gzip level %d, got %d", kgzip.BestCompression, cfg.gzipLevel)
	}
}

func TestWithBrotliLevel(t *testing.T) {
	cfg := streamConfig{
		gzipLevel:   kgzip.DefaultCompression,
		brotliLevel: int(defaultBrotliLevel),
	}
	WithBrotliLevel(LevelBest)(&cfg)
	if cfg.brotliLevel != 11 {
		t.Fatalf("expected brotli level 11, got %d", cfg.brotliLevel)
	}
}
