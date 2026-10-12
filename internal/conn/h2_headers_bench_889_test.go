package conn

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris/internal/protocol/h2/frame"
)

// BenchmarkProcessH2Headers889 measures ProcessH2 on the path every request of
// a native HTTP/2 connection takes: a plain HEADERS frame (END_HEADERS,
// END_STREAM) through hasCompleteFrame, the frame-size comparison of the fast
// path and the processor's admission, answered inline. celeris#889 changed the
// frame size that comparison reads (the server's advertised one) and what
// hasCompleteFrame is given. "single" is one frame per call, the common
// event-loop read; "pipelined8" eight, so the loop's "more frames follow" case
// is included. The server's output is discarded.
func BenchmarkProcessH2Headers889(b *testing.B) {
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", "/"}, {":authority", "example.com"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	block := append([]byte(nil), hb.Bytes()...)

	var tmpl bytes.Buffer
	if err := http2.NewFramer(&tmpl, nil).WriteRawFrame(http2.FrameHeaders,
		http2.FlagHeadersEndHeaders|http2.FlagHeadersEndStream, 1, block); err != nil {
		b.Fatal(err)
	}
	one := tmpl.Bytes()

	for _, tc := range []struct {
		name string
		n    int
	}{{"single", 1}, {"pipelined8", 8}} {
		b.Run(tc.name, func(b *testing.B) {
			// An empty body: the answer is one HEADERS frame with END_STREAM, so the
			// connection's send window is never spent (a body would use it up and,
			// with no WINDOW_UPDATE from this client, leave every later stream open
			// until the stream limit refuses it).
			h := respondHandler{}
			write := func([]byte) {}
			cfg := H2Config{}
			state := NewH2State(h, cfg, write, nil)
			var pre bytes.Buffer
			pre.WriteString(frame.ClientPreface)
			if err := http2.NewFramer(&pre, nil).WriteSettings(); err != nil {
				b.Fatal(err)
			}
			if err := ProcessH2(context.Background(), pre.Bytes(), state, h, write, cfg); err != nil {
				b.Fatal(err)
			}
			buf := make([]byte, 0, tc.n*len(one))
			for range tc.n {
				buf = append(buf, one...)
			}
			id := uint32(1)
			b.ReportAllocs()
			for b.Loop() {
				for i := range tc.n {
					binary.BigEndian.PutUint32(buf[i*len(one)+5:], id)
					id += 2
				}
				if err := ProcessH2(context.Background(), buf, state, h, write, cfg); err != nil {
					b.Fatal(err)
				}
			}
			// Every request was opened and answered, none refused: a refusal is cheap
			// and would make the figure the cost of something else.
			mgr := state.processor.GetManager()
			if got, want := mgr.GetLastStreamID(), id-2; got != want || mgr.CountActiveStreams() != 0 {
				b.Fatalf("last stream opened %d (want %d), %d streams still active (want 0): requests were refused or left open", got, want, mgr.CountActiveStreams())
			}
		})
	}
}
