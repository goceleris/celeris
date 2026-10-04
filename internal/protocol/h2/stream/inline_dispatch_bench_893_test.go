package stream

import (
	"context"
	"testing"
)

// BenchmarkInlineDispatch893 measures what celeris#893 adds to the dispatch of
// an inline-eligible stream (a GET with END_STREAM on a sync route): one
// atomic load of the connection's held outbound bytes before the handler runs
// inline. The handler writes nothing, so the stream is answered and released
// on the inline path every iteration.
func BenchmarkInlineDispatch893(b *testing.B) {
	h := HandlerFunc(func(context.Context, *Stream) error { return nil })
	proc := NewProcessor(h, newTestFrameWriter(), newTestResponseWriter())
	proc.InlineWriter = newTestResponseWriter()
	id := uint32(1)
	b.ReportAllocs()
	for b.Loop() {
		s := proc.manager.CreateStream(id)
		s.EndStream = true
		id += 2
		proc.runHandler(s)
	}
}
