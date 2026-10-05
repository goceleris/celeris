package stream

import (
	"context"
	"testing"
	"time"
)

// What celeris#836 costs on the inline H2 dispatch (see
// BenchmarkInlineDispatch893 for the handler that asks for nothing). Each use
// of a stream now gets its own context, made the first time the use asks for
// it, instead of a view of the pooled Stream.

// BenchmarkInlineDispatchAskContext836: the handler asks for its stream's
// context and reads Err, as c.Context() in a middleware does.
func BenchmarkInlineDispatchAskContext836(b *testing.B) {
	h := HandlerFunc(func(_ context.Context, s *Stream) error {
		_ = s.Context().Err()
		return nil
	})
	benchInlineDispatch836(b, h)
}

// BenchmarkInlineDispatchDeriveContext836: the handler derives a context
// from its stream's and cancels it, as context.WithTimeout(c.Context(), d)
// with defer cancel() does (the shape that panicked).
func BenchmarkInlineDispatchDeriveContext836(b *testing.B) {
	h := HandlerFunc(func(_ context.Context, s *Stream) error {
		ctx, cancel := context.WithTimeout(s.Context(), time.Hour)
		cancel()
		_ = ctx
		return nil
	})
	benchInlineDispatch836(b, h)
}

func benchInlineDispatch836(b *testing.B, h Handler) {
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
