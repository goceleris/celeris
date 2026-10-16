package compress

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

type mark888 struct {
	size int
	ct   string
}

func (m mark888) CompressibleRepresentation() (int, string) { return m.size, m.ct }

// TestCompressibleKeyIsPinned888: static and etag set the key as a literal in
// their own packages (this module cannot import an internal package the
// released core lacks), and each package's tests pin it; this pins compress's.
func TestCompressibleKeyIsPinned888(t *testing.T) {
	if compressibleKey != "celeris.compressible-representation" {
		t.Fatalf("compressibleKey = %q: static and etag set %q", compressibleKey, "celeris.compressible-representation")
	}
	if compressActiveKey != "celeris.compress.active" {
		t.Fatalf("compressActiveKey = %q: static and etag look for %q", compressActiveKey, "celeris.compress.active")
	}
}

// TestCompressSaysWhenItIsActive888: compress sets compressActiveKey, which
// static and etag look for before they mark a response, exactly when it has
// negotiated an encoding and runs the handlers below it buffered: not for a
// request without Accept-Encoding, for HEAD, OPTIONS, a Skip, a streaming
// request, or an Accept-Encoding none of whose codings it offers.
func TestCompressSaysWhenItIsActive888(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		cfg    Config
		hdrs   [][2]string
		want   bool
	}{
		{"gzip", "GET", Config{}, [][2]string{{"accept-encoding", "gzip"}}, true},
		{"POST", "POST", Config{}, [][2]string{{"accept-encoding", "gzip"}}, true},
		{"no Accept-Encoding", "GET", Config{}, nil, false},
		{"identity", "GET", Config{}, [][2]string{{"accept-encoding", "identity"}}, false},
		{"refuses gzip", "GET", Config{}, [][2]string{{"accept-encoding", "gzip;q=0"}}, false},
		{"HEAD", "HEAD", Config{}, [][2]string{{"accept-encoding", "gzip"}}, false},
		{"OPTIONS", "OPTIONS", Config{}, [][2]string{{"accept-encoding", "gzip"}}, false},
		{"skipped", "GET", Config{Skip: func(*celeris.Context) bool { return true }}, [][2]string{{"accept-encoding", "gzip"}}, false},
		{"no encoding in common", "GET", Config{Encodings: []string{"zstd"}}, [][2]string{{"accept-encoding", "gzip"}}, false},
		{"event stream", "GET", Config{}, [][2]string{{"accept-encoding", "gzip"}, {"accept", "text/event-stream"}}, false},
		{"websocket", "GET", Config{}, [][2]string{{"accept-encoding", "gzip"}, {"upgrade", "websocket"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			h := func(c *celeris.Context) error {
				_, got = c.Get(compressActiveKey)
				return c.String(200, "ok")
			}
			opts := []celeristest.Option{celeristest.WithHandlers(newCompress888(tc.cfg), h)}
			for _, hd := range tc.hdrs {
				opts = append(opts, celeristest.WithHeader(hd[0], hd[1]))
			}
			c, _ := celeristest.NewContextT(t, tc.method, "/", opts...)
			if err := c.Next(); err != nil {
				t.Fatalf("Next: %v", err)
			}
			if got != tc.want {
				t.Errorf("the handler below compress sees the key: %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMarkedNotModifiedFollowsTheRule888: what compress does with a 304 whose
// handler marked the representation (the marker's size and content type stand
// in for the body and type the 304 lacks), and what it leaves alone.
func TestMarkedNotModifiedFollowsTheRule888(t *testing.T) {
	notModified := func(tag string, m any, extra ...[2]string) celeris.HandlerFunc {
		return func(c *celeris.Context) error {
			if tag != "" {
				c.SetHeader("etag", tag)
			}
			for _, h := range extra {
				c.SetHeader(h[0], h[1])
			}
			if m != nil {
				c.Set(compressibleKey, m)
			}
			return c.NoContent(304)
		}
	}
	for _, tc := range []struct {
		name string
		cfg  Config
		ae   string
		h    celeris.HandlerFunc
		want string
	}{
		{"marked, encodable", Config{}, "gzip", notModified(`"h"`, mark888{5000, "text/plain"}), `W/"h"`},
		{"marked, size at MinLength", Config{MinLength: 5000}, "gzip", notModified(`"h"`, mark888{5000, "text/plain"}), `W/"h"`},
		{"marked, size under MinLength", Config{MinLength: 5001}, "gzip", notModified(`"h"`, mark888{5000, "text/plain"}), `"h"`},
		{"marked, MinLength 0 encodes any non-empty", Config{MinLength: 0}, "gzip", notModified(`"h"`, mark888{1, "text/plain"}), `W/"h"`},
		{"marked, empty", Config{}, "gzip", notModified(`"h"`, mark888{0, "text/plain"}), `"h"`},
		{"marked, excluded type", Config{}, "gzip", notModified(`"h"`, mark888{5000, "image/png"}), `"h"`},
		{"marked, excluded type in another case", Config{}, "gzip", notModified(`"h"`, mark888{5000, "Image/PNG"}), `"h"`},
		{"marked, no exclusions", Config{ExcludedContentTypes: []string{}}, "gzip", notModified(`"h"`, mark888{5000, "image/png"}), `W/"h"`},
		{"marked, no encoding in common", Config{Encodings: []string{"zstd"}}, "gzip", notModified(`"h"`, mark888{5000, "text/plain"}), `"h"`},
		{"marked, no Accept-Encoding", Config{}, "", notModified(`"h"`, mark888{5000, "text/plain"}), `"h"`},
		{"marked, weak tag stays as it is", Config{}, "gzip", notModified(`W/"h"`, mark888{5000, "text/plain"}), `W/"h"`},
		{"marked, the handler's own Content-Encoding", Config{}, "gzip", notModified(`"h"`, mark888{5000, "text/plain"}, [2]string{"content-encoding", "br"}), `"h"`},
		{"marked, no tag", Config{}, "gzip", notModified("", mark888{5000, "text/plain"}), ""},
		{"not marked", Config{}, "gzip", notModified(`"h"`, nil), `"h"`},
		{"a value that is not a marker", Config{}, "gzip", notModified(`"h"`, "yes"), `"h"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaultConfig
			if tc.cfg.MinLength != 0 || tc.name == "marked, MinLength 0 encodes any non-empty" {
				cfg.MinLength = tc.cfg.MinLength
			}
			if tc.cfg.Encodings != nil {
				cfg.Encodings = tc.cfg.Encodings
			}
			if tc.cfg.ExcludedContentTypes != nil {
				cfg.ExcludedContentTypes = tc.cfg.ExcludedContentTypes
			}
			opts := []celeristest.Option{celeristest.WithHandlers(New(cfg), tc.h)}
			if tc.ae != "" {
				opts = append(opts, celeristest.WithHeader("accept-encoding", tc.ae))
			}
			c, rec := celeristest.NewContextT(t, "GET", "/", opts...)
			if err := c.Next(); err != nil {
				t.Fatalf("Next: %v", err)
			}
			if rec.StatusCode != 304 || rec.Header("etag") != tc.want {
				t.Errorf("status %d etag %q, want 304 with %q", rec.StatusCode, rec.Header("etag"), tc.want)
			}
			if rec.Header("content-encoding") != "" && tc.name != "marked, the handler's own Content-Encoding" {
				t.Errorf("a 304 carries content-encoding %q", rec.Header("content-encoding"))
			}
		})
	}
}

// TestMarkedResponseSentAsItIsKeepsTheWeakTag888: a marked 200 compress does
// not send encoded because the encoded form is longer (or compression failed)
// carries the weak tag, as its 304 does, which cannot know the encoded
// length (RFC 9110 15.4.5); an unmarked one keeps the strong tag it had.
func TestMarkedResponseSentAsItIsKeepsTheWeakTag888(t *testing.T) {
	noise := make([]byte, 2000)
	var x uint32 = 1
	for i := range noise {
		x = x*1664525 + 1013904223
		noise[i] = byte(x >> 24)
	}
	h := func(mark bool) celeris.HandlerFunc {
		return func(c *celeris.Context) error {
			c.SetHeader("etag", `"h"`)
			if mark {
				c.Set(compressibleKey, mark888{len(noise), "application/octet-stream"})
			}
			return c.Blob(200, "application/octet-stream", noise)
		}
	}
	for _, tc := range []struct {
		name string
		mark bool
		want string
	}{{"marked", true, `W/"h"`}, {"unmarked", false, `"h"`}} {
		c, rec := celeristest.NewContextT(t, "GET", "/", celeristest.WithHeader("accept-encoding", "gzip"),
			celeristest.WithHandlers(newCompress888(Config{}), h(tc.mark)))
		if err := c.Next(); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if rec.Header("content-encoding") != "" || rec.Header("etag") != tc.want || len(rec.Body) != len(noise) {
			t.Errorf("%s: content-encoding %q etag %q (%d bytes), want identity %d bytes with %q", tc.name, rec.Header("content-encoding"), rec.Header("etag"), len(rec.Body), len(noise), tc.want)
		}
	}
}
