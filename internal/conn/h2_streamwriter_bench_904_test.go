package conn

import (
	"bytes"
	"context"
	"testing"

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
						mgr.UpdateConnectionWindow(1 << 21)
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
			st = NewH2State(h, H2Config{}, func([]byte) {}, nil)
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
