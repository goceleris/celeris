package stream

import (
	"bytes"
	"context"
	"testing"

	"golang.org/x/net/http2"
)

// BenchmarkSplitHeadersFirstFragment889 is the first fragment of a split
// header block through ProcessFrame, with and without the PRIORITY flag
// (celeris#889 item 29): the block is begun and dropped, as the allocation
// test does. The two differ by one allocation when the priority field is held
// by pointer.
func BenchmarkSplitHeadersFirstFragment889(b *testing.B) {
	block := newEnc981().req()
	for _, withPriority := range []bool{false, true} {
		name := "plain"
		if withPriority {
			name = "priority"
		}
		b.Run(name, func(b *testing.B) {
			var wire bytes.Buffer
			param := http2.HeadersFrameParam{StreamID: 1, BlockFragment: block[:len(block)/2], EndStream: true}
			if withPriority {
				param.Priority = http2.PriorityParam{StreamDep: 0, Weight: 15}
			}
			if err := http2.NewFramer(&wire, nil).WriteHeaders(param); err != nil {
				b.Fatal(err)
			}
			raw := wire.Bytes()
			p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error { return nil }),
				newTestFrameWriter(), &rstConn944{testResponseWriter: newTestResponseWriter()})
			rd := bytes.NewReader(raw)
			fr := http2.NewFramer(nil, rd)
			fr.SetReuseFrames()
			fr.SetMaxReadFrameSize(1 << 20)
			fr.AllowIllegalReads = true // the same HEADERS again, with no CONTINUATION between
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				rd.Reset(raw)
				f, err := fr.ReadFrame()
				if err != nil {
					b.Fatal(err)
				}
				if err := p.ProcessFrame(ctx, f); err != nil {
					b.Fatal(err)
				}
				p.continuationStateMu.Lock()
				p.continuationState = nil
				p.continuationActive.Store(false)
				p.continuationStateMu.Unlock()
			}
		})
	}
}
