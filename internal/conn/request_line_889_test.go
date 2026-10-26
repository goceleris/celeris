package conn

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/goceleris/celeris/internal/protocol/h1"
)

// celeris#889, items 40 and 45: a request line with a method that is not a
// token, or a request-target with a control byte, is a 400 and the connection
// is closed, on the first request of a connection and on a later one on the
// same keep-alive connection (detect.Detect looks at the first byte of the
// connection only).
func TestProcessH1_InvalidRequestLine889(t *testing.T) {
	const ok = "GET /ok HTTP/1.1\r\nHost: x\r\n\r\n"
	for _, tc := range []struct{ name, line string }{
		{"method starts with LF", "\nGET /p HTTP/1.1"},
		{"method has NUL", "G\x00T /p HTTP/1.1"},
		{"method is empty", " /p HTTP/1.1"},
		{"target has NUL", "GET /p\x00e=gzip HTTP/1.1"},
		{"target has bare CR", "GET /p\re HTTP/1.1"},
		{"target has bare LF", "GET /p\ne HTTP/1.1"},
	} {
		for _, pos := range []string{"first", "second"} {
			t.Run(pos+"/"+tc.name, func(t *testing.T) {
				state := NewH1State()
				h := &recordingHandler{}
				var writes [][]byte
				write := func(b []byte) { writes = append(writes, append([]byte(nil), b...)) }
				bad := tc.line + "\r\nHost: x\r\n\r\n"
				data := bad
				if pos == "second" {
					// The first request is served and the connection kept alive.
					if err := ProcessH1(context.Background(), []byte(ok), state, h, write); err != nil {
						t.Fatalf("first request: %v", err)
					}
					if !h.called {
						t.Fatal("first request not served")
					}
					h.called = false
					writes = nil
				}
				err := ProcessH1(context.Background(), []byte(data), state, h, write)
				if !errors.Is(err, h1.ErrInvalidRequestLine) {
					t.Fatalf("err = %v, want ErrInvalidRequestLine", err)
				}
				if h.called {
					t.Fatal("the handler ran for an invalid request line")
				}
				if len(writes) != 1 || !bytes.HasPrefix(writes[0], []byte("HTTP/1.1 400 ")) {
					t.Fatalf("writes = %q, want one 400", writes)
				}
			})
		}
	}
}
