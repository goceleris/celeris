package conn

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris/protocol/h2/frame"
	"github.com/goceleris/celeris/protocol/h2/stream"
)

// inlineStreamerHandler records the writer it was handed: whether it is the
// direct-to-outBuf inline adapter (the #408 optimization) and whether it is a
// stream.Streamer (SSE-safety), then writes a buffered response.
type inlineStreamerHandler struct {
	isInline   *bool
	streamerOK *bool
}

func (h inlineStreamerHandler) HandleStream(_ context.Context, s *stream.Stream) error {
	if s.ResponseWriter == nil {
		return nil
	}
	_, *h.isInline = s.ResponseWriter.(*h2InlineResponseAdapter)
	_, *h.streamerOK = s.ResponseWriter.(stream.Streamer)
	return s.ResponseWriter.WriteResponse(s, 200, [][2]string{{"content-type", "text/plain"}}, []byte("ok"))
}

// TestH2InlineResponse_DirectOutBuf_StaysStreamer is the regression guard for
// celeris#408. Realizing the inline-outBuf bypass must (a) actually take the
// direct-to-outBuf path (skip the sharded queue + eventfd self-wake), and
// (b) keep the inline ResponseWriter a stream.Streamer — otherwise
// Context.StreamWriter() returns nil and SSE/chunked-over-H2 hard-500s.
//
//	old connWriter path:  streamer=true,  queue.pending=TRUE  → fails (b)-check (not optimized)
//	naive InlineWriter:   streamer=FALSE, queue.pending=false → fails (a)-check (SSE broken)
//	this fix:             streamer=true,  queue.pending=false → passes both
func TestH2InlineResponse_DirectOutBuf_StaysStreamer(t *testing.T) {
	var isInline, streamerOK bool
	h := inlineStreamerHandler{isInline: &isInline, streamerOK: &streamerOK}
	var mu sync.Mutex
	var writes []byte
	write := func(b []byte) { mu.Lock(); writes = append(writes, b...); mu.Unlock() }

	state := NewH2State(h, H2Config{}, write, -1) // wakeupFD=-1: no eventfd

	// Client preface + empty SETTINGS + a single inline-eligible GET (END_STREAM).
	hdr := encodeH2Headers(t, [][2]string{
		{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", "x"},
	})
	var in bytes.Buffer
	in.WriteString(frame.ClientPreface)
	fr := http2.NewFramer(&in, nil)
	if err := fr.WriteSettings(); err != nil {
		t.Fatalf("WriteSettings: %v", err)
	}
	if err := fr.WriteRawFrame(http2.FrameHeaders,
		http2.FlagHeadersEndStream|http2.FlagHeadersEndHeaders, 1, hdr); err != nil {
		t.Fatalf("WriteHeaders: %v", err)
	}
	if err := ProcessH2(context.Background(), in.Bytes(), state, h, write, H2Config{}); err != nil {
		t.Fatalf("ProcessH2: %v", err)
	}

	// (a) the optimization: the inline handler must be handed the direct-to-outBuf
	// InlineWriter, not the queue-backed connWriter (else the Enqueue + eventfd
	// self-wake the inline path was meant to skip still fire).
	if !isInline {
		t.Fatal("inline handler's ResponseWriter is not the direct-outBuf InlineWriter (celeris#408 optimization not applied)")
	}
	// (b) SSE-safety: the inline writer MUST be a stream.Streamer, else
	// Context.StreamWriter() returns nil and SSE/chunked-over-H2 hard-500s.
	if !streamerOK {
		t.Fatal("inline handler's ResponseWriter is not a stream.Streamer — Context.StreamWriter()/SSE over H2 would 500 (celeris#408)")
	}
	if len(writes) == 0 {
		t.Fatal("no bytes written for the inline GET response")
	}
}
