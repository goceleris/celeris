package conn

import (
	"context"
	"testing"

	"golang.org/x/net/http2"

	"github.com/goceleris/celeris/internal/protocol/h2/stream"
)

// A streamed response its handler broke off, with the failure swallowed above
// the processor (stream.FailResponse: the router recovers a panic and cannot
// write its 500 over a response that is under way), returns from HandleStream
// as if it had finished. With no content-length declared it would look
// complete; it is reset instead (celeris#889).
func TestStreamedResponseBrokenOffIsResetNotEnded889(t *testing.T) {
	for _, arm := range arms889() {
		t.Run(arm.name, func(t *testing.T) {
			c := newH2Return889(t, arm.h(func(ctx context.Context, s *stream.Stream) error {
				if err := streamed889(-1, 5)(ctx, s); err != nil {
					return err
				}
				s.FailResponse()
				return nil
			}), H2Config{})
			c.req("GET", nil, 1)
			c.settle()
			w := c.stream(1)
			if w.ended || len(w.rst) != 1 || w.rst[0] != http2.ErrCodeInternal || w.seq[len(w.seq)-1] != "RST_STREAM" {
				t.Errorf("ended=%v rst=%v (%s), want one INTERNAL_ERROR RST_STREAM as the last frame and no END_STREAM", w.ended, w.rst, w.summary())
			}
		})
	}
}

// FailResponse changes nothing for a response written whole or already closed.
func TestFailResponseLeavesAWholeResponseAlone889(t *testing.T) {
	for _, arm := range arms889() {
		t.Run(arm.name+"/whole", func(t *testing.T) {
			c := newH2Return889(t, arm.h(func(_ context.Context, s *stream.Stream) error {
				_ = s.ResponseWriter.WriteResponse(s, 200, nil, []byte("ok"))
				s.FailResponse()
				return nil
			}), H2Config{})
			c.req("GET", nil, 1)
			c.settle()
			if w := c.stream(1); !w.ended || len(w.rst) != 0 {
				t.Errorf("ended=%v rst=%v (%s), want the response kept", w.ended, w.rst, w.summary())
			}
		})
		t.Run(arm.name+"/closed", func(t *testing.T) {
			c := newH2Return889(t, arm.h(func(ctx context.Context, s *stream.Stream) error {
				if err := streamed889(-1, 5)(ctx, s); err != nil {
					return err
				}
				_ = s.ResponseWriter.(stream.Streamer).Close(s)
				s.FailResponse()
				return nil
			}), H2Config{})
			c.req("GET", nil, 1)
			c.settle()
			if w := c.stream(1); !w.ended || len(w.rst) != 0 {
				t.Errorf("ended=%v rst=%v (%s), want the closed response kept", w.ended, w.rst, w.summary())
			}
		})
	}
}
