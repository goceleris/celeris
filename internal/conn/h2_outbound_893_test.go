package conn

import (
	"context"
	"testing"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

type nopHandler893 struct{}

func (nopHandler893) HandleStream(context.Context, *stream.Stream) error { return nil }

// TestH2ResponseFrameBufferIsSizedForWhatIsSent893: celeris#893. A pool
// handler's WriteResponse allocated its frame buffer for HEADERS plus the
// WHOLE body, though the windows let it put only part of the body in it (none,
// with the connection window used up). Every stream of a client that grants
// no window then queued a body-sized buffer.
func TestH2ResponseFrameBufferIsSizedForWhatIsSent893(t *testing.T) {
	for _, hdrs := range [][][2]string{
		{{"content-type", "text/plain"}, {"content-length", "1048576"}}, // the pre-encoded fast path
		{{"content-type", "application/octet-stream"}},                  // the HPACK encoder path
	} {
		st := NewH2State(nopHandler893{}, H2Config{}, func([]byte) {}, nil)
		mgr := st.processor.GetManager()
		s := mgr.CreateStream(1)
		s.SetState(stream.StateOpen)
		s.SetWindowSize(65535)
		mgr.UpdateConnectionWindow(-mgr.GetConnectionWindow()) // the peer has granted nothing more
		body := make([]byte, 1<<20)
		if err := st.adapter.WriteResponse(s, 200, hdrs, body); err != nil {
			t.Fatal(err)
		}
		var frames []*[]byte
		for i := range st.writeQueue.shards {
			frames = append(frames, st.writeQueue.shards[i].bufs...)
		}
		if len(frames) != 1 {
			t.Fatalf("%v: %d frame buffers queued, want 1 (the HEADERS)", hdrs[0], len(frames))
		}
		if c := cap(*frames[0]); c >= 64<<10 {
			t.Errorf("%v: the queued HEADERS frame buffer has capacity %d for %d bytes of HEADERS and no DATA (the body is %d bytes)",
				hdrs[0], c, len(*frames[0]), len(body))
		}
		buffered := -1
		if s.OutboundBuffer != nil {
			buffered = s.OutboundBuffer.Len()
		}
		if buffered != len(body) {
			t.Errorf("%v: the stream buffered %d bytes, want the whole body (%d) for the window", hdrs[0], buffered, len(body))
		}
	}
}

// TestH2QueueDrainDropsDrainedFrames893: after DrainTo, the queue keeps no
// pointer to a frame buffer it has drained. putH2FrameBuf does not pool a
// buffer past 8 KiB, and a pointer left in a shard's spare array kept such a
// buffer (a response's DATA) reachable until a later drain overwrote its slot.
func TestH2QueueDrainDropsDrainedFrames893(t *testing.T) {
	var q h2ShardedQueue
	for i := uint32(0); i < 2*h2QueueShards; i++ {
		big := make([]byte, 0, 64<<10)
		q.Enqueue(2*i+1, &big)
	}
	q.DrainTo(func([]byte) {})
	for k := range q.shards {
		sp := q.shards[k].spare
		for j, p := range sp[:cap(sp)] {
			if p != nil {
				t.Errorf("shard %d: spare slot %d still points at a drained %d-byte-capacity frame buffer", k, j, cap(*p))
			}
		}
	}
}
