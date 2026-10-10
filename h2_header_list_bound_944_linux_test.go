//go:build linux

package celeris_test

import (
	"bytes"
	"net"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/goceleris/celeris"
)

// celeris#944, review round 1: joining the cookie fields must not let one
// small HEADERS frame become a huge Cookie header. One literal cookie of
// about 4 KB, indexed into the HPACK dynamic table, then one-byte references
// (0xBE) to it: a 16 KB frame, which any peer may send, decoded to a 49 MB
// Cookie on epoll, io_uring and adaptive. net/http (std) answers it 431.

// outcome944 reads frames off fr until stream id has a response (its HEADERS,
// whose :status it returns) or is reset (RST_STREAM), and returns what
// happened as a string: "status 200", "status 431", "RST ENHANCE_YOUR_CALM",
// or "GOAWAY ...". dec decodes the response HEADERS; one per connection.
func outcome944(t *testing.T, fr *http2.Framer, dec *hpack.Decoder, id uint32) string {
	t.Helper()
	var status string
	dec.SetEmitFunc(func(f hpack.HeaderField) {
		if f.Name == ":status" {
			status = f.Value
		}
	})
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("stream %d: reading the response: %v", id, err)
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				_ = fr.WriteSettingsAck()
			}
		case *http2.HeadersFrame:
			if f.StreamID != id {
				continue
			}
			status = ""
			if _, err := dec.Write(f.HeaderBlockFragment()); err != nil {
				t.Fatal(err)
			}
			if status != "" {
				return "status " + status
			}
		case *http2.RSTStreamFrame:
			if f.StreamID == id {
				return "RST " + f.ErrCode.String()
			}
		case *http2.GoAwayFrame:
			return "GOAWAY " + f.ErrCode.String() + " " + string(f.DebugData())
		}
	}
}

func TestH2HugeHeaderListIsRefused944(t *testing.T) {
	const wantStatus = "status 431"
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			var longest, runs atomic.Int64
			addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
				s.GET("/c", func(c *celeris.Context) error {
					runs.Add(1)
					if n := int64(len(c.Header("cookie"))); n > longest.Load() {
						longest.Store(n)
					}
					return c.String(200, "ok")
				})
			})
			conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
			if _, err := conn.Write([]byte(http2.ClientPreface)); err != nil {
				t.Fatal(err)
			}
			fr := http2.NewFramer(conn, conn)
			_ = fr.WriteSettings()
			dec := hpack.NewDecoder(4096, nil)

			var hb bytes.Buffer
			enc := hpack.NewEncoder(&hb)
			fields := func() {
				for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", addr}, {":path", "/c"}} {
					_ = enc.WriteField(hpack.HeaderField{Name: f[0], Value: f[1]})
				}
			}
			big := strings.Repeat("a", 4000)

			// Stream 1: the 4 KB cookie, then 12288 one-byte references to it.
			fields()
			_ = enc.WriteField(hpack.HeaderField{Name: "cookie", Value: big})
			hb.Write(bytes.Repeat([]byte{0xBE}, 12<<10))
			block := append([]byte(nil), hb.Bytes()...)
			if len(block) > 16384 {
				t.Fatalf("the block is %d bytes: not inside the default 16 KiB frame", len(block))
			}
			runtime.GC()
			var a, b runtime.MemStats
			runtime.ReadMemStats(&a)
			if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: block, EndStream: true, EndHeaders: true}); err != nil {
				t.Fatal(err)
			}
			got := outcome944(t, fr, dec, 1)
			runtime.ReadMemStats(&b)
			alloc := b.TotalAlloc - a.TotalAlloc
			t.Logf("%s: %d-byte frame -> %s; handler runs %d, longest Cookie %d bytes; process allocated %d bytes (%.0fx)",
				e.name, len(block), got, runs.Load(), longest.Load(), alloc, float64(alloc)/float64(len(block)))

			// net/http answers 431; the native engines reset the stream.
			if got == "status 200" || (got != wantStatus && !strings.HasPrefix(got, "RST ")) {
				t.Errorf("answer %q, want %q or a RST_STREAM", got, wantStatus)
			}
			if r, n := runs.Load(), longest.Load(); r != 0 || n != 0 {
				t.Errorf("the handler ran %d times and saw a Cookie of %d bytes", r, n)
			}
			if alloc > 16<<20 {
				t.Errorf("a %d-byte frame made the process allocate %d bytes (%.0fx)", len(block), alloc, float64(alloc)/float64(len(block)))
			}

			// The connection serves the next request, whose cookie is the
			// entry the refused block added to the HPACK table: the decoder
			// finished the block and stayed in step with the peer's encoder.
			hb.Reset()
			fields()
			_ = enc.WriteField(hpack.HeaderField{Name: "cookie", Value: big})
			if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 3, BlockFragment: append([]byte(nil), hb.Bytes()...), EndStream: true, EndHeaders: true}); err != nil {
				t.Fatal(err)
			}
			if next := outcome944(t, fr, dec, 3); next != "status 200" {
				t.Errorf("the next request on the connection: %s, want status 200", next)
			}
			if r, n := runs.Load(), longest.Load(); r != 1 || n != 4000 {
				t.Errorf("after the next request: the handler ran %d times, longest Cookie %d bytes; want 1 and 4000", r, n)
			}
		})
	}
}
