package stream

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/net/http2"
)

// celeris#889, item 45: RFC 9113 section 8.2.1: "A field value MUST NOT
// contain the zero value (ASCII NUL, 0x00), line feed (ASCII LF, 0x0a), or
// carriage return (ASCII CR, 0x0d) at any position." An HPACK string can hold
// any octet, so the decoder hands such a value over; a request that has one
// is malformed (section 8.1.1) and is refused with a stream error.

func TestValidateRequestHeadersRefusesNULCRLF889(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field [2]string
	}{
		{":path with NUL", [2]string{":path", "/p\x00e=gzip"}},
		{":path with LF", [2]string{":path", "/p\ne"}},
		{":path with CR", [2]string{":path", "/p\re"}},
		{":authority with NUL", [2]string{":authority", "a\x00b"}},
		{":method with LF", [2]string{":method", "G\nT"}},
		{"regular value with NUL", [2]string{"x-a", "a\x00b"}},
		{"regular value with CR LF", [2]string{"x-a", "a\r\nb: c"}},
		{"cookie with CR LF", [2]string{"cookie", "a=b\r\nset-cookie: x=y"}},
		{"value that is one NUL", [2]string{"x-a", "\x00"}},
		{"value ending in LF", [2]string{"x-a", "abc\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hs [][2]string
			for _, f := range reqHeaders944() {
				if f[0] == tc.field[0] {
					f = tc.field
				}
				hs = append(hs, f)
			}
			if tc.field[0][0] != ':' {
				hs = append(hs, tc.field)
			}
			if err := validateRequestHeaders(hs); err == nil {
				t.Fatal("a request with a NUL, CR or LF in a field value was accepted")
			}
		})
	}
	// Controls: other bytes are not this rule's concern, and a request
	// without any of the three passes.
	for _, v := range []string{"", "a b", "tab\there", "caf\xc3\xa9", "\x01\x1f", "\x7f", "a%00b", "a\\r\\nb"} {
		hs := append(reqHeaders944(), [2]string{"x-a", v})
		if err := validateRequestHeaders(hs); err != nil {
			t.Errorf("value %q refused: %v", v, err)
		}
	}
	if err := validateRequestHeaders(reqHeaders944()); err != nil {
		t.Fatalf("a plain request refused: %v", err)
	}
}

func TestValidateTrailerHeadersRefusesNULCRLF889(t *testing.T) {
	for _, v := range []string{"a\x00b", "a\nb", "a\rb"} {
		if err := validateTrailerHeaders([][2]string{{"x-t", v}}); err == nil {
			t.Errorf("trailer value %q accepted", v)
		}
	}
	if err := validateTrailerHeaders([][2]string{{"x-t", "a b"}}); err != nil {
		t.Errorf("plain trailer refused: %v", err)
	}
}

// Through the processor, on both HEADERS entry points: the stream gets
// RST_STREAM(PROTOCOL_ERROR), the handler does not run, and the same request
// without the byte is served.
func TestProcessorRefusesNULCRLFInRequest889(t *testing.T) {
	for _, path := range []string{"raw", "frame"} {
		for _, tc := range []struct{ name, key, val string }{
			{"path NUL", ":path", "/p\x00e=gzip"},
			{"path LF", ":path", "/p\ne"},
			{"value NUL", "x-a", "a\x00b"},
			{"cookie CRLF", "cookie", "a=b\r\nset-cookie: x=y"},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				var runs atomic.Int32
				fw := newTestFrameWriter()
				p := NewProcessor(HandlerFunc(func(_ context.Context, _ *Stream) error {
					runs.Add(1)
					return nil
				}), fw, newTestResponseWriter())
				block := func(clean bool) []byte {
					hs := reqHeaders944()
					v := tc.val
					if clean {
						v = strings.NewReplacer("\x00", "", "\n", "", "\r", "").Replace(v)
					}
					if tc.key == ":path" {
						hs[2][1] = v
					} else {
						hs = append(hs, [2]string{tc.key, v})
					}
					return encodeHeaders(t, hs)
				}
				err := feedRaw944(t, p, path, 1, true, block(false))
				if err == nil {
					t.Fatal("the request was not refused")
				}
				fw.mu.Lock()
				rst := append([]rstStreamRecord(nil), fw.rstStreamsSent...)
				fw.mu.Unlock()
				if len(rst) != 1 || rst[0].streamID != 1 || rst[0].code != http2.ErrCodeProtocol {
					t.Fatalf("RST_STREAMs %+v, want one PROTOCOL_ERROR on stream 1", rst)
				}
				if n := runs.Load(); n != 0 {
					t.Fatalf("the handler ran %d times for a malformed request", n)
				}
				// Control: without the bytes, the request is served.
				p2 := NewProcessor(HandlerFunc(func(_ context.Context, _ *Stream) error {
					runs.Add(1)
					return nil
				}), newTestFrameWriter(), newTestResponseWriter())
				if err := feedRaw944(t, p2, path, 1, true, block(true)); err != nil {
					t.Fatalf("clean request: %v", err)
				}
				if n := runs.Load(); n != 1 {
					t.Fatalf("clean request: handler ran %d times, want 1", n)
				}
			})
		}
	}
}

func BenchmarkValidateRequestHeaders889(b *testing.B) {
	hs := append(reqHeaders944(),
		[2]string{"user-agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"},
		[2]string{"accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		[2]string{"accept-encoding", "gzip, deflate, br"},
		[2]string{"accept-language", "en-US,en;q=0.9"},
		[2]string{"cookie", "session=0123456789abcdef0123456789abcdef; theme=dark; csrf=fedcba9876543210"},
	)
	b.ReportAllocs()
	for b.Loop() {
		if err := validateRequestHeaders(hs); err != nil {
			b.Fatal(err)
		}
	}
}
