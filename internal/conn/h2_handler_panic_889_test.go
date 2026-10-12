package conn

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris/internal/protocol/h2/frame"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#889 item 31: H2Config.Logger gets an Error record for a panic that
// escapes an HTTP/2 stream's handler (it was dropped without a trace).

type panicStreamHandler889 struct{}

func (panicStreamHandler889) HandleStream(context.Context, *stream.Stream) error {
	panic("boom889")
}

// logBuf889 is a slog sink that keeps what it is given.
type logBuf889 struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuf889) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuf889) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestH2HandlerPanicIsLogged889(t *testing.T) {
	var sink logBuf889
	cfg := H2Config{Logger: slog.New(slog.NewTextHandler(&sink, nil))}
	write := func([]byte) {}
	h := panicStreamHandler889{}
	state := NewH2State(h, cfg, write, nil)

	var in bytes.Buffer
	in.WriteString(frame.ClientPreface)
	fr := http2.NewFramer(&in, nil)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	hdr := encodeH2Headers(t, [][2]string{
		{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", "x"},
	})
	if err := fr.WriteRawFrame(http2.FrameHeaders, http2.FlagHeadersEndStream|http2.FlagHeadersEndHeaders, 1, hdr); err != nil {
		t.Fatal(err)
	}
	if err := ProcessH2(context.Background(), in.Bytes(), state, h, write, cfg); err != nil {
		t.Fatalf("a handler panic ended the connection: %v", err)
	}
	out := sink.String()
	for _, want := range []string{"level=ERROR", "h2 handler panicked", "stream=1", "panic=boom889", "panicStreamHandler889"} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %q:\n%s", want, out)
		}
	}
	if got := state.processor.HandlerPanics(); got != 1 {
		t.Errorf("HandlerPanics = %d, want 1", got)
	}
}

// Without a Logger nothing is logged and nothing breaks.
func TestH2HandlerPanicWithoutALoggerIsStillCounted889(t *testing.T) {
	write := func([]byte) {}
	h := panicStreamHandler889{}
	state := NewH2State(h, H2Config{}, write, nil)
	var in bytes.Buffer
	in.WriteString(frame.ClientPreface)
	fr := http2.NewFramer(&in, nil)
	if err := fr.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	hdr := encodeH2Headers(t, [][2]string{
		{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", "x"},
	})
	if err := fr.WriteRawFrame(http2.FrameHeaders, http2.FlagHeadersEndStream|http2.FlagHeadersEndHeaders, 1, hdr); err != nil {
		t.Fatal(err)
	}
	if err := ProcessH2(context.Background(), in.Bytes(), state, h, write, H2Config{}); err != nil {
		t.Fatal(err)
	}
	if got := state.processor.HandlerPanics(); got != 1 {
		t.Errorf("HandlerPanics = %d, want 1", got)
	}
}
