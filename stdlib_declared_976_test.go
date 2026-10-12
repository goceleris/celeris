package celeris

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// celeris#889 item 25, the ToHandler sibling of the std bridge's check: a
// Content-Length over the 100 MB ToHandler body cap is refused with 413 before
// the body is read (it used to be buffered to the cap first), and on HTTP/1 the
// answer says Connection: close, as the body is left unread.

type countedBody976 struct {
	r     io.Reader
	reads int
}

func (b *countedBody976) Read(p []byte) (int, error) { b.reads++; return b.r.Read(p) }
func (b *countedBody976) Close() error               { return nil }

func TestToHandlerDeclaredLengthDecidesBeforeTheRead976(t *testing.T) {
	for _, major := range []int{1, 2} {
		for _, tc := range []struct {
			name      string
			cl        int64
			body      int
			status    int
			wantReads bool
			ran       bool
		}{
			{"declared-over", maxToHandlerBodySize + 1, 0, 413, false, false},
			{"declared-far-over", 1 << 40, 0, 413, false, false},
			{"declared-at-cap", maxToHandlerBodySize, 3, 200, true, true},
			{"declared-small", 3, 3, 200, true, true},
			{"unknown-length", -1, 3, 200, true, true},
		} {
			name := tc.name
			t.Run(map[int]string{1: "h1-", 2: "h2-"}[major]+name, func(t *testing.T) {
				ran := false
				h := ToHandler(func(c *Context) error { ran = true; return c.String(200, "ok") })
				body := &countedBody976{r: bytes.NewReader(make([]byte, tc.body))}
				req := httptest.NewRequest(http.MethodPost, "/", nil)
				req.Body, req.ContentLength, req.ProtoMajor = body, tc.cl, major
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != tc.status || ran != tc.ran {
					t.Fatalf("status %d (handler ran: %v), want %d (%v)", rec.Code, ran, tc.status, tc.ran)
				}
				if (body.reads > 0) != tc.wantReads {
					t.Fatalf("the body was read %d times, want reads: %v", body.reads, tc.wantReads)
				}
				if tc.status == 413 {
					if got, want := rec.Header().Get("Connection") == "close", major == 1; got != want {
						t.Fatalf("Connection: close = %v on HTTP/%d, want %v", got, major, want)
					}
				}
			})
		}
	}
}
