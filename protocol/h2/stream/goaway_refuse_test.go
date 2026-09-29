package stream

import (
	"context"
	"errors"
	"sync"
	"testing"

	"golang.org/x/net/http2"
)

// rstRecordingConn is a testResponseWriter that records the RST_STREAM
// frames written through it, and fails them with err when it is set.
type rstRecordingConn struct {
	*testResponseWriter
	mu  sync.Mutex
	rst []rstStreamRecord
	err error
}

func (c *rstRecordingConn) WriteRSTStreamPriority(streamID uint32, code http2.ErrCode) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rst = append(c.rst, rstStreamRecord{streamID: streamID, code: code})
	return c.err
}

// TestRefusedStreamIsLeftAloneOnceReleased: runHandler refuses a stream the
// client opened above the last stream a GOAWAY named (celeris#759), and the
// refusal deletes the stream, which puts it back in the stream pool. Nothing
// may touch it after that: another connection can have taken it from the
// pool by then, and a write to it closed that connection's stream, or, with
// its new ID, deleted the stream of this connection that had the same ID.
// The object's state after the call shows such a write: resetAndPool left
// it at ID 0 and StateIdle.
func TestRefusedStreamIsLeftAloneOnceReleased(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"rst-sent", nil},
		{"rst-failed", errors.New("write failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &rstRecordingConn{testResponseWriter: newTestResponseWriter(), err: tc.err}
			ran := make(chan struct{}, 1)
			p := NewProcessor(HandlerFunc(func(context.Context, *Stream) error {
				ran <- struct{}{}
				return nil
			}), newTestFrameWriter(), conn)
			if err := p.SendGoAway(1, http2.ErrCodeNo, nil); err != nil {
				t.Fatalf("SendGoAway: %v", err)
			}
			s := p.manager.CreateStream(3)
			p.runHandler(s)

			if _, ok := p.manager.GetStream(3); ok {
				t.Fatal("refused stream 3 is still in the manager")
			}
			conn.mu.Lock()
			rst := append([]rstStreamRecord(nil), conn.rst...)
			conn.mu.Unlock()
			if len(rst) != 1 || rst[0].streamID != 3 || rst[0].code != http2.ErrCodeRefusedStream {
				t.Fatalf("RST_STREAM frames %+v, want one REFUSED_STREAM for stream 3", rst)
			}
			select {
			case <-ran:
				t.Fatal("the handler of refused stream 3 ran")
			default:
			}
			if id, st := s.ID, State(s.state.Load()); id != 0 || st != StateIdle {
				t.Fatalf("stream 3's object after its release: ID %d, state %v; want ID 0, %v: it was written after it went back to the pool", id, st, StateIdle)
			}
		})
	}
}
