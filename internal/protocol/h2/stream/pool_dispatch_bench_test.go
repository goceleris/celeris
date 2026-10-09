package stream

import (
	"context"
	"testing"
)

// doneHandler signals every stream it has handled; the benchmark waits for
// it so each iteration is one full dispatch round trip.
type doneHandler struct{ done chan struct{} }

func (h *doneHandler) HandleStream(_ context.Context, _ *Stream) error {
	h.done <- struct{}{}
	return nil
}

// BenchmarkPoolDispatch measures one stream handed to the shared worker pool
// (runHandler → Submit → executeHandler), the path whose per-connection
// handler count celeris#759 adds (an atomic add before Submit and one after
// executeHandler returns). The stream is not END_STREAM, so it is not
// eligible to run inline.
func BenchmarkPoolDispatch(b *testing.B) {
	h := &doneHandler{done: make(chan struct{}, 1)}
	proc := NewProcessor(h, newTestFrameWriter(), newTestResponseWriter())
	id := uint32(1)
	b.ReportAllocs()
	for b.Loop() {
		s := proc.manager.CreateStream(id)
		id += 2
		proc.runHandler(s)
		<-h.done
	}
}
