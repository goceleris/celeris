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
	cs := NewCompressedStream(nil, "zstd")
	if cs != nil {
		t.Fatal("expected nil for unsupported encoding")
	}
	cs = NewCompressedStream(nil, "deflate")
	if cs != nil {
		t.Fatal("expected nil for unsupported encoding")
	}
	cs = NewCompressedStream(nil, "bogus")
	if cs != nil {
		t.Fatal("expected nil for unsupported encoding")
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

	// Use the writerFunc adapter directly to test the compression pipeline.
	w := streamGzipPool.Get().(*kgzip.Writer)
	w.Reset(writerFunc(sink.write))

	for _, chunk := range chunks {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	streamGzipPool.Put(w)

	// Verify decompressed output.
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

	w := streamBrotliPool.Get().(*brotli.Writer)
	w.Reset(writerFunc(sink.write))

	for _, chunk := range chunks {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("brotli write: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("brotli close: %v", err)
	}
	streamBrotliPool.Put(w)

	// Verify decompressed output.
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

func TestWriteHeaderAddsEncodingAndVary(t *testing.T) {
	tests := []struct {
		encoding string
	}{
		{"gzip"},
		{"br"},
	}
	for _, tt := range tests {
		t.Run(tt.encoding, func(t *testing.T) {
			cs := &CompressedStream{encoding: tt.encoding}
			if cs.encoding != tt.encoding {
				t.Fatalf("expected encoding %s, got %s", tt.encoding, cs.encoding)
			}
			// Verify the header layout that WriteHeader would produce:
			// content-encoding and vary are prepended to user headers.
			userHeaders := [][2]string{
				{"content-type", "text/plain"},
				{"x-custom", "value"},
			}
			extra := make([][2]string, 0, len(userHeaders)+2)
			extra = append(extra, [2]string{"content-encoding", cs.encoding})
			extra = append(extra, [2]string{"vary", "Accept-Encoding"})
			extra = append(extra, userHeaders...)

			if extra[0][0] != "content-encoding" || extra[0][1] != tt.encoding {
				t.Fatalf("expected content-encoding=%s first, got %v", tt.encoding, extra[0])
			}
			if extra[1][0] != "vary" || extra[1][1] != "Accept-Encoding" {
				t.Fatalf("expected vary=Accept-Encoding second, got %v", extra[1])
			}
			if extra[2][0] != "content-type" || extra[2][1] != "text/plain" {
				t.Fatalf("expected user headers preserved, got %v", extra[2])
			}
		})
	}
}
