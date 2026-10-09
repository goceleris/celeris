package conn

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// celeris#947: a stream that completes inline while more frames follow in the
// same read has its map removal and Release deferred to FlushInlineCleanup,
// which kept only the *Stream. If the peer's RST_STREAM for it comes in that
// read, DeleteStream releases the stream to the pool at once, and a HEADERS
// later in the read can get the same object back from NewStream, with a new
// ID. The deferred cleanup then found "its" stream in the map under that new
// ID and released it while the new stream's pool handler was still running:
// the handler's context was cancelled with no RST from the client, its
// response was lost, and the handler's own completion released the object a
// second time, so two later streams, on any connection, could share it.

// reuseHandler947 answers /a inline at once; /b is an async route whose
// handler waits for the test.
type reuseHandler947 struct {
	mu      sync.Mutex
	a, b    *stream.Stream
	bCtx    context.Context
	bStart  chan struct{}
	bGo     chan struct{}
	bDone   chan struct{}
	bLostRW bool
}

func (h *reuseHandler947) HandleStream(_ context.Context, s *stream.Stream) error {
	var path string
	for _, hf := range s.Headers {
		if hf[0] == ":path" {
			path = hf[1]
		}
	}
	if path == "/a" {
		h.mu.Lock()
		h.a = s
		h.mu.Unlock()
		return s.ResponseWriter.WriteResponse(s, 200, nil, []byte("a"))
	}
	defer close(h.bDone)
	h.mu.Lock()
	h.b, h.bCtx = s, s.Context()
	h.mu.Unlock()
	close(h.bStart)
	<-h.bGo
	rw := s.ResponseWriter
	if rw == nil {
		h.mu.Lock()
		h.bLostRW = true
		h.mu.Unlock()
		return nil
	}
	return rw.WriteResponse(s, 200, nil, []byte("b"))
}

func (h *reuseHandler947) RouteAsync(_, path string) bool { return path == "/b" }
func (h *reuseHandler947) HasAsyncRoutes() bool           { return true }

// TestInlineCleanupDoesNotReleaseAReusedStream947 sends, in one read,
// HEADERS(1, /a), RST_STREAM(1) and HEADERS(3, /b), as one client write
// does. Whether stream 3 gets stream 1's object is up to the stream pool, so
// the read is repeated on fresh connections until it has, several times.
func TestInlineCleanupDoesNotReleaseAReusedStream947(t *testing.T) {
	hdr := func(path string) []byte {
		var b bytes.Buffer
		enc := hpack.NewEncoder(&b)
		for _, hf := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: path}} {
			_ = enc.WriteField(hf)
		}
		return b.Bytes()
	}
	var in bytes.Buffer
	in.WriteString(http2.ClientPreface)
	fr := http2.NewFramer(&in, nil)
	_ = fr.WriteSettings()
	_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hdr("/a"), EndStream: true, EndHeaders: true})
	_ = fr.WriteRSTStream(1, http2.ErrCodeCancel)
	_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 3, BlockFragment: hdr("/b"), EndStream: true, EndHeaders: true})
	read := in.Bytes()

	const wantReuse = 5
	reused := 0
	for attempt := 0; attempt < 400 && reused < wantReuse; attempt++ {
		h := &reuseHandler947{bStart: make(chan struct{}), bGo: make(chan struct{}), bDone: make(chan struct{})}
		var mu sync.Mutex
		var wire bytes.Buffer
		write := func(b []byte) { mu.Lock(); wire.Write(b); mu.Unlock() }
		st := NewH2State(h, H2Config{}, write, nil)
		ctx := context.Background()
		if err := ProcessH2(ctx, read, st, h, write, H2Config{}); err != nil {
			t.Fatal(err)
		}
		// ProcessH2 has run FlushInlineCleanup: stream 3's handler is still
		// waiting.
		select {
		case <-h.bStart:
		case <-time.After(5 * time.Second):
			t.Fatal("stream 3's handler did not start")
		}
		h.mu.Lock()
		same := h.a == h.b
		bCtx := h.bCtx
		h.mu.Unlock()
		if same {
			reused++
			if err := bCtx.Err(); err != nil {
				t.Fatalf("attempt %d: stream 3 took stream 1's pooled object, and stream 1's deferred cleanup cancelled it (Err %v) while its handler ran; the client never reset stream 3", attempt, err)
			}
			if got, ok := st.processor.GetManager().GetStream(3); !ok || got != h.b {
				t.Fatalf("attempt %d: stream 3 took stream 1's pooled object, and stream 1's deferred cleanup removed it from the connection while its handler ran", attempt)
			}
		}
		close(h.bGo)
		<-h.bDone
		// Let the pool goroutine finish the stream, then flush its response.
		deadline := time.Now().Add(5 * time.Second)
		var answered bool
		for !answered && time.Now().Before(deadline) {
			st.DrainWriteQueue(write)
			mu.Lock()
			out := append([]byte(nil), wire.Bytes()...)
			mu.Unlock()
			rf := http2.NewFramer(nil, bytes.NewReader(out))
			for {
				f, err := rf.ReadFrame()
				if err != nil {
					break
				}
				if d, ok := f.(*http2.DataFrame); ok && d.StreamID == 3 && d.StreamEnded() {
					answered = true
				}
			}
			if !answered {
				time.Sleep(time.Millisecond)
			}
		}
		h.mu.Lock()
		lost := h.bLostRW
		h.mu.Unlock()
		if same && (lost || !answered) {
			t.Fatalf("attempt %d: stream 3 took stream 1's pooled object and its response was lost (no response writer: %v)", attempt, lost)
		}
		if !answered {
			t.Fatalf("attempt %d: stream 3 was not answered", attempt)
		}
		for st.processor.PoolHandlersRunning() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		CloseH2(st)
	}
	if reused < wantReuse {
		t.Fatalf("stream 3 got stream 1's pooled object in %d reads, want %d: this test did not test the reuse", reused, wantReuse)
	}
}
