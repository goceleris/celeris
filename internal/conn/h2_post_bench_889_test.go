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

// BenchmarkProcessH2Post889 is BenchmarkProcessH2Headers889 for a request with a
// body: a HEADERS frame without END_STREAM and a DATA frame with it (16 bytes),
// answered inline. This is the path the request deadline (celeris#889) touches:
// the HEADERS arm it (one clock read, a plain store, a flag, an armed count and
// the hint) and the END_STREAM disarms it; a GET (the benchmark next to it) does
// neither. "single" is one request per call, "pipelined8" eight.
func BenchmarkProcessH2Post889(b *testing.B) {
	var hb bytes.Buffer
	enc := hpack.NewEncoder(&hb)
	for _, f := range [][2]string{{":method", "POST"}, {":scheme", "http"}, {":path", "/"}, {":authority", "example.com"}, {"content-length", "16"}} {
		_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
	}
	var tmpl bytes.Buffer
	fr := http2.NewFramer(&tmpl, nil)
	if err := fr.WriteRawFrame(http2.FrameHeaders, http2.FlagHeadersEndHeaders, 1, hb.Bytes()); err != nil {
		b.Fatal(err)
	}
	hdrLen := tmpl.Len()
	if err := fr.WriteData(1, true, make([]byte, 16)); err != nil {
		b.Fatal(err)
	}
	one := append([]byte(nil), tmpl.Bytes()...)
	dataOff := hdrLen

	for _, tc := range []struct {
		name string
		n    int
	}{{"single", 1}, {"pipelined8", 8}} {
		b.Run(tc.name, func(b *testing.B) {
			h := respondHandler{}
			write := func([]byte) {}
			cfg := benchCfg889()
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
					binary.BigEndian.PutUint32(buf[i*len(one)+dataOff+5:], id)
					id += 2
				}
				if err := ProcessH2(context.Background(), buf, state, h, write, cfg); err != nil {
					b.Fatal(err)
				}
			}
			mgr := state.processor.GetManager()
			if got, want := mgr.GetLastStreamID(), id-2; got != want || mgr.CountActiveStreams() != 0 {
				b.Fatalf("last stream opened %d (want %d), %d streams still active (want 0): requests were refused or left open", got, want, mgr.CountActiveStreams())
			}
		})
	}
}
