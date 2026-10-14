//go:build linux

package celeris_test

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
)

// TestH2InlineCleanupDoesNotReleaseAReusedStream947 is celeris#947 on the
// wire: one client write carries HEADERS(1) for a sync route, RST_STREAM(1)
// and HEADERS(3) for an async route. Stream 1 completes inline with its
// cleanup deferred to the end of the read, the RST releases it at once, and
// stream 3 can get the same pooled object. Before the fix the deferred
// cleanup then released stream 3 while its handler ran: its context was
// cancelled with no RST from the client, and its response never came. std is
// the control (net/http's HTTP/2 server).
func TestH2InlineCleanupDoesNotReleaseAReusedStream947(t *testing.T) {
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			var handled, cancelled atomic.Int64
			addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
				s.GET("/a", func(c *celeris.Context) error { return c.String(200, "a") })
				s.GET("/b", func(c *celeris.Context) error {
					ctx := c.Context()
					time.Sleep(20 * time.Millisecond)
					handled.Add(1)
					if ctx.Err() != nil {
						cancelled.Add(1)
					}
					return c.String(200, "b")
				}).Async()
			})
			hdr := func(path string) []byte {
				var b bytes.Buffer
				enc := hpack.NewEncoder(&b)
				for _, hf := range []hpack.HeaderField{{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"}, {Name: ":authority", Value: addr}, {Name: ":path", Value: path}} {
					_ = enc.WriteField(hf)
				}
				return b.Bytes()
			}
			var w bytes.Buffer
			w.WriteString(http2.ClientPreface)
			fr := http2.NewFramer(&w, nil)
			_ = fr.WriteSettings()
			_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: hdr("/a"), EndStream: true, EndHeaders: true})
			_ = fr.WriteRSTStream(1, http2.ErrCodeCancel)
			_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 3, BlockFragment: hdr("/b"), EndStream: true, EndHeaders: true})
			one := w.Bytes()

			conns := n836(80) / 4 // 20, or 5 under -race and coverage
			unanswered := 0
			var first string
			for i := range conns {
				conn, err := net.Dial("tcp", addr)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := conn.Write(one); err != nil {
					t.Fatal(err)
				}
				_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				rd := http2.NewFramer(nil, conn)
				answered := false
				var seen []string
				for !answered {
					f, err := rd.ReadFrame()
					if err != nil {
						seen = append(seen, err.Error())
						break
					}
					seen = append(seen, f.Header().String())
					if d, ok := f.(*http2.DataFrame); ok && d.StreamID == 3 && d.StreamEnded() {
						answered = true
					}
				}
				_ = conn.Close()
				if !answered {
					unanswered++
					if first == "" {
						first = fmt.Sprintf("conn %d: %s", i, strings.Join(seen, " | "))
					}
				}
			}
			if unanswered != 0 {
				t.Errorf("%d of %d connections never got stream 3's response; first: %s", unanswered, conns, first)
			}
			// Each handler that ran has counted itself before its response.
			if c := cancelled.Load(); c != 0 {
				t.Errorf("%d of %d stream-3 handlers saw their context cancelled; the client never reset stream 3", c, handled.Load())
			}
		})
	}
}
