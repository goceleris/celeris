package conn

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"runtime"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// The end of a stream's use (celeris#904, review of celeris#965): what a
// StreamWriter may still do once its handler returned or its stream was reset.

// TestCloseAfterWriteTimeoutQueuesNothing904: a pool StreamWriter whose Write
// timed out has had its stream reset (RST_STREAM INTERNAL_ERROR). The Close
// that `defer sw.Close()` makes next must not queue DATA(0) with END_STREAM
// after it: RFC 9113 §5.1 allows no frame on a closed stream, and a strict
// peer answers STREAM_CLOSED.
func TestCloseAfterWriteTimeoutQueuesNothing904(t *testing.T) {
	done := make(chan struct{ werr, cerr error }, 1)
	c := newFlushConn(t, &asyncHandler893{run: func(_ context.Context, s *stream.Stream) error {
		sw := s.ResponseWriter.(stream.Streamer)
		_ = sw.WriteHeader(s, 200, [][2]string{{"content-type", "text/plain"}})
		werr := sw.Write(s, make([]byte, 5000))
		cerr := sw.Close(s)
		done <- struct{ werr, cerr error }{werr, cerr}
		return nil
	}}, H2Config{WriteTimeout: 50 * time.Millisecond})
	c.open(0, []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1000}}, 1)
	var r struct{ werr, cerr error }
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return")
	}
	c.drain()
	if !errors.Is(r.werr, os.ErrDeadlineExceeded) {
		t.Fatalf("Write returned %v, want the WriteTimeout error", r.werr)
	}
	if r.cerr == nil {
		t.Error("Close after the stream was reset returned nil, want an error")
	}
	w := c.stream(1)
	if len(w.rst) != 1 || w.seq[len(w.seq)-1] != "RST_STREAM" {
		t.Errorf("stream 1 on the wire: %s, want the RST_STREAM last: no frame may follow it", w.summary())
	}
}

// TestUnclosedInlineStreamWriterIsDrainedAndReleased904: an inline handler
// streams more than the window allows and returns without closing its
// StreamWriter. The buffered rest is still sent as the peer grants window, but
// without END_STREAM (a truncated body must not look complete), and the stream
// is then released: it must not hold its slot and its share of the
// connection's outbound budget until the connection closes.
func TestUnclosedInlineStreamWriterIsDrainedAndReleased904(t *testing.T) {
	body := pattern904(5000)
	h := &syncHandler893{run: func(_ context.Context, s *stream.Stream) error {
		sw := s.ResponseWriter.(stream.Streamer)
		if err := sw.WriteHeader(s, 200, nil); err != nil {
			return err
		}
		return sw.Write(s, body) // never closed
	}}
	c := newFlushConn(t, h, H2Config{})
	c.open(0, []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1000}}, 1)
	c.drain()
	m := c.st.processor.GetManager()
	if got := m.StreamCount(); got != 1 {
		t.Fatalf("with 4000 bytes buffered, the manager has %d streams, want the one that holds them", got)
	}
	if held := m.OutboundHeld(); held != 4000 {
		t.Fatalf("the connection holds %d buffered bytes, want 4000", held)
	}
	c.grant(1<<20, true, 1)
	c.drain()
	w := c.stream(1)
	if !bytes.Equal(w.data, body) {
		t.Errorf("%d DATA bytes on the wire, want the whole %d-byte body, in order (frames %s)", len(w.data), len(body), w.summary())
	}
	if w.ended {
		t.Errorf("END_STREAM was sent for a response its handler never ended (frames %s)", w.summary())
	}
	if got := m.StreamCount(); got != 0 {
		t.Errorf("the manager still has %d stream(s) after the buffer was sent: the stream holds its slot until the connection closes", got)
	}
	if held := m.OutboundHeld(); held != 0 {
		t.Errorf("the connection still holds %d buffered bytes", held)
	}
}

// TestReserveSendClampsHugeRequests904: h2ReserveSend (a StreamWriter's and
// sendRest's reservation) takes a request of 2 GiB or more as MaxInt32. int32(n)
// wrapped to zero or a negative number, which ReserveSendWindow refuses, and a
// pool Write of that size then looped for ever with the windows open.
func TestReserveSendClampsHugeRequests904(t *testing.T) {
	if math.MaxInt == math.MaxInt32 {
		t.Skip("int is 32 bits")
	}
	m := stream.NewManager()
	m.UpdateConnectionWindow(math.MaxInt32 - 65535)
	s := m.CreateStream(1)
	s.SetWindowSize(math.MaxInt32)
	huge := int(math.MaxInt32) + 1
	if got := h2ReserveSend(m, s, huge); got != math.MaxInt32 {
		t.Errorf("h2ReserveSend of %d with both windows at MaxInt32 reserved %d, want %d", huge, got, math.MaxInt32)
	}
	m2 := stream.NewManager()
	s2 := m2.CreateStream(1)
	if got := h2ReserveSend(m2, s2, 3<<30); got != 65535 {
		t.Errorf("h2ReserveSend of 3 GiB with default windows reserved %d, want 65535", got)
	}
	if got := h2ReserveSend(nil, s2, huge); got != 0 {
		t.Errorf("h2ReserveSend without a manager and an emptied stream window reserved %d, want 0", got)
	}
}

// TestOneByteGrantOnTheWriteQueuePathDoesNotCopyTheBody911: the production
// path of a one-byte grant, with the conn layer's outbound sink (ProcessH2,
// the sink, dataFrames, the write queue), as the stream package's tests cannot
// see it: they run on a Processor with no sink. The bytes allocated per grant
// must stay far below the buffered body.
func TestOneByteGrantOnTheWriteQueuePathDoesNotCopyTheBody911(t *testing.T) {
	const tail = 4 << 20
	const grants = 200
	body := pattern904(65535 + tail)
	discard := func([]byte) {}
	done := make(chan struct{})
	h := &asyncHandler893{run: func(_ context.Context, s *stream.Stream) error {
		defer close(done)
		return s.ResponseWriter.WriteResponse(s, 200, nil, body)
	}}
	st := NewH2State(h, H2Config{}, discard, nil)
	defer st.processor.GetManager().Close()
	open := append([]byte(http2.ClientPreface), frames893(t, func(fr *http2.Framer, enc *hpack.Encoder, hb *bytes.Buffer) {
		_ = fr.WriteSettings()
		for _, hf := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/b"}} {
			_ = enc.WriteField(hf)
		}
		_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
	})...)
	if err := ProcessH2(context.Background(), open, st, h, discard, H2Config{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not finish")
	}
	waitFlush(t, "the tail to be buffered", func() bool { return st.processor.GetManager().OutboundHeld() == tail })
	st.DrainWriteQueue(discard)
	grant := frames893(t, func(fr *http2.Framer, _ *hpack.Encoder, _ *bytes.Buffer) {
		_ = fr.WriteWindowUpdate(1, 1)
		_ = fr.WriteWindowUpdate(0, 1)
	})
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	for i := 0; i < grants; i++ {
		if err := ProcessH2(context.Background(), grant, st, h, discard, H2Config{}); err != nil {
			t.Fatal(err)
		}
		st.DrainWriteQueue(discard)
	}
	runtime.ReadMemStats(&b)
	per := float64(b.TotalAlloc-a.TotalAlloc) / grants
	t.Logf("%d one-byte grants with a %d-byte tail buffered: %.0f bytes allocated per grant", grants, tail, per)
	if held := st.processor.GetManager().OutboundHeld(); held != tail-grants {
		t.Errorf("the connection holds %d buffered bytes, want %d: each grant must have sent exactly its one byte", held, tail-grants)
	}
	if per > 8192 {
		t.Errorf("%.0f bytes allocated per one-byte grant with a %d-byte tail buffered, want well under 8192: the rest of the body is copied for every frame", per, tail)
	}
}
