package conn

import (
	"bytes"
	"context"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// BenchmarkStreamWriterWrite904 is the cost of one 1 KiB StreamWriter Write
// (celeris#904 gives it flow control: both windows reserved, a wait on a pool
// goroutine, and under the stream's lock when it runs on the event loop). The
// windows are topped up and the queue drained every 1024 writes in both arms,
// outside any difference between them. pool = an async route's goroutine,
// inline = a sync handler on the event loop (the lock).
func BenchmarkStreamWriterWrite904(b *testing.B) {
	for _, mode := range []string{"pool", "inline"} {
		b.Run(mode, func(b *testing.B) {
			chunk := make([]byte, 1024)
			done := make(chan struct{})
			var st *H2State
			run := func(_ context.Context, s *stream.Stream) error {
				defer close(done)
				sw := s.ResponseWriter.(stream.Streamer)
				if err := sw.WriteHeader(s, 200, nil); err != nil {
					return err
				}
				mgr := st.processor.GetManager()
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if i&1023 == 0 {
						s.SetWindowSize(1 << 30)
						mgr.UpdateConnectionWindow(1<<21 - mgr.GetConnectionWindow())
						st.DrainWriteQueue(func([]byte) {})
					}
					if err := sw.Write(s, chunk); err != nil {
						return err
					}
				}
				b.StopTimer()
				return nil
			}
			var h stream.Handler = &asyncHandler893{run: run}
			if mode == "inline" {
				h = &syncHandler893{run: run}
			}
			// The server's default WriteTimeout (60 s), which is what a pool
			// Write's deadline is computed from: a bench with none would not
			// measure the clock reads the deadline can cost.
			st = NewH2State(h, H2Config{WriteTimeout: time.Minute}, func([]byte) {}, nil)
			b.Cleanup(func() { st.processor.GetManager().Close() })
			in := append([]byte(http2.ClientPreface), frames893(&testing.T{}, func(fr *http2.Framer, enc *hpack.Encoder, hb *bytes.Buffer) {
				_ = fr.WriteSettings()
				hb.Reset()
				for _, hf := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/b"}} {
					_ = enc.WriteField(hf)
				}
				_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
			})...)
			if err := ProcessH2(context.Background(), in, st, h, func([]byte) {}, H2Config{}); err != nil {
				b.Fatal(err)
			}
			<-done
		})
	}
}

// BenchmarkWriteResponsePool904 is the cost of WriteResponse (one 1 KiB body)
// from a pool goroutine: the hot response path of an async route, which this
// change reorders (HEADERS and the first DATA are queued before the rest is
// buffered) and gives a test seam (h2BeforeEnqueueHook, one load and a nil
// check). The same stream answers every iteration, with the windows and the
// queue topped up every 1024, as in BenchmarkStreamWriterWrite904.
func BenchmarkWriteResponsePool904(b *testing.B) {
	body := make([]byte, 1024)
	done := make(chan struct{})
	var st *H2State
	run := func(_ context.Context, s *stream.Stream) error {
		defer close(done)
		mgr := st.processor.GetManager()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if i&1023 == 0 {
				s.SetWindowSize(1 << 30)
				mgr.UpdateConnectionWindow(1<<21 - mgr.GetConnectionWindow())
				st.DrainWriteQueue(func([]byte) {})
			}
			if err := s.ResponseWriter.WriteResponse(s, 200, nil, body); err != nil {
				return err
			}
		}
		b.StopTimer()
		return nil
	}
	h := &asyncHandler893{run: run}
	st = NewH2State(h, H2Config{WriteTimeout: time.Minute}, func([]byte) {}, nil)
	b.Cleanup(func() { st.processor.GetManager().Close() })
	in := append([]byte(http2.ClientPreface), frames893(&testing.T{}, func(fr *http2.Framer, enc *hpack.Encoder, hb *bytes.Buffer) {
		_ = fr.WriteSettings()
		hb.Reset()
		for _, hf := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: "x"}, {Name: ":path", Value: "/b"}} {
			_ = enc.WriteField(hf)
		}
		_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hb.Bytes(), EndStream: true, EndHeaders: true})
	})...)
	if err := ProcessH2(context.Background(), in, st, h, func([]byte) {}, H2Config{}); err != nil {
		b.Fatal(err)
	}
	<-done
}
