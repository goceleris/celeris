package conn

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/protocol/h2/frame"
	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// BenchmarkStreamedResponse889 is a whole StreamWriter response of an inline
// handler, from the HEADERS frame to the stream's release: WriteHeader with a
// content-length, one 5-byte Write, then Close ("closed", the common case: the
// handler did what the API asks) or no Close ("unclosed": the processor ends the
// stream at the handler's return, celeris#889). celeris#889 adds to this path
// the stream's bookkeeping of the response (NoteStreamHeader, the byte count in
// StreamWrite, the ended mark in EndOutbound) and, at the return, one check of
// a flag; the unclosed arm also pays for the END_STREAM it sends. The server's
// output is drained and discarded every iteration.
func BenchmarkStreamedResponse889(b *testing.B) {
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", "example.com"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	var tmpl bytes.Buffer
	if err := http2.NewFramer(&tmpl, nil).WriteRawFrame(http2.FrameHeaders,
		http2.FlagHeadersEndHeaders|http2.FlagHeadersEndStream, 1, hb.Bytes()); err != nil {
		b.Fatal(err)
	}
	one := tmpl.Bytes()
	body := []byte("hello")
	for _, closed := range []bool{true, false} {
		name := "unclosed"
		if closed {
			name = "closed"
		}
		b.Run(name, func(b *testing.B) {
			h := &syncHandler893{run: func(_ context.Context, s *stream.Stream) error {
				sw := s.ResponseWriter.(stream.Streamer)
				if err := sw.WriteHeader(s, 200, [][2]string{{"content-length", "5"}}); err != nil {
					return err
				}
				if err := sw.Write(s, body); err != nil {
					return err
				}
				if closed {
					return sw.Close(s)
				}
				return nil
			}}
			write := func([]byte) {}
			cfg := H2Config{}
			state := NewH2State(h, cfg, write, nil)
			defer state.processor.GetManager().Close()
			var pre bytes.Buffer
			pre.WriteString(frame.ClientPreface)
			if err := http2.NewFramer(&pre, nil).WriteSettings(); err != nil {
				b.Fatal(err)
			}
			if err := ProcessH2(context.Background(), pre.Bytes(), state, h, write, cfg); err != nil {
				b.Fatal(err)
			}
			buf := append([]byte(nil), one...)
			id := uint32(1)
			mgr := state.processor.GetManager()
			b.ReportAllocs()
			for b.Loop() {
				if id&2047 == 1 {
					// The client sends no WINDOW_UPDATE: top the connection's
					// window up, or the 5-byte bodies would spend it and later
					// responses would be buffered.
					mgr.UpdateConnectionWindow(1<<21 - mgr.GetConnectionWindow())
				}
				binary.BigEndian.PutUint32(buf[5:], id)
				id += 2
				if err := ProcessH2(context.Background(), buf, state, h, write, cfg); err != nil {
					b.Fatal(err)
				}
				state.DrainWriteQueue(write)
			}
			if mgr.CountActiveStreams() != 0 || mgr.OutboundHeld() != 0 {
				b.Fatalf("%d streams still active, %d bytes buffered: responses were left open or held back", mgr.CountActiveStreams(), mgr.OutboundHeld())
			}
		})
	}
}
