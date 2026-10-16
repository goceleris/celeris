package etag

import (
	"testing"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
)

// celeris#888 item 36: etag marks a response whose strong tag compress would
// weaken, so that compress gives the 304 the tag the 200 carries. The key is
// the one compress and static use (compress cannot import it: a separate
// module).
func TestStrongTagIsMarkedForCompress888(t *testing.T) {
	const key = "celeris.compressible-representation"
	body := []byte("0123456789012345678901234567890123456789")
	blob := func(c *celeris.Context) error { return c.Blob(200, "text/plain; charset=utf-8", body) }
	ownTag := func(tag string) celeris.HandlerFunc {
		return func(c *celeris.Context) error { c.SetHeader("etag", tag); return blob(c) }
	}
	partial := func(c *celeris.Context) error {
		c.SetHeader("etag", `"v1"`)
		c.SetHeader("content-range", "bytes 0-9/40")
		return c.Blob(206, "text/plain", body[:10])
	}
	for _, tc := range []struct {
		name     string
		cfg      Config
		h        celeris.HandlerFunc
		method   string
		hdrs     [][2]string
		noActive bool // compress is not in front: it did not set its key
		wantMark bool
		wantCode int
	}{
		{"strong computed", Config{Strong: true}, blob, "GET", [][2]string{{"accept-encoding", "gzip"}}, false, true, 200},
		{"strong computed, 304", Config{Strong: true}, blob, "GET", [][2]string{{"accept-encoding", "gzip"}, {"if-none-match", "*"}}, false, true, 304},
		{"handler's strong tag", Config{}, ownTag(`"v1"`), "GET", [][2]string{{"accept-encoding", "gzip"}}, false, true, 200},
		{"handler's strong tag, 304", Config{}, ownTag(`"v1"`), "GET", [][2]string{{"accept-encoding", "gzip"}, {"if-none-match", `"v1"`}}, false, true, 304},
		{"weak computed (default)", Config{}, blob, "GET", [][2]string{{"accept-encoding", "gzip"}}, false, false, 200},
		{"handler's weak tag", Config{}, ownTag(`W/"v1"`), "GET", [][2]string{{"accept-encoding", "gzip"}}, false, false, 200},
		{"compress not in front", Config{Strong: true}, blob, "GET", [][2]string{{"accept-encoding", "gzip"}}, true, false, 200},
		{"compress not in front, 304", Config{Strong: true}, blob, "GET", [][2]string{{"accept-encoding", "gzip"}, {"if-none-match", "*"}}, true, false, 304},
		{"a 206", Config{}, partial, "GET", [][2]string{{"accept-encoding", "gzip"}}, false, false, 206},
		{"a 304 of a 206", Config{}, partial, "GET", [][2]string{{"accept-encoding", "gzip"}, {"if-none-match", `"v1"`}}, false, false, 304},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// compress in front: it sets the key etag looks for when it has
			// negotiated an encoding (this package cannot import compress).
			active := func(c *celeris.Context) error {
				if !tc.noActive {
					c.Set("celeris.compress.active", true)
				}
				return c.Next()
			}
			opts := []celeristest.Option{celeristest.WithHandlers(active, New(tc.cfg), tc.h)}
			for _, h := range tc.hdrs {
				opts = append(opts, celeristest.WithHeader(h[0], h[1]))
			}
			c, rec := celeristest.NewContextT(t, tc.method, "/", opts...)
			if err := c.Next(); err != nil {
				t.Fatalf("Next: %v", err)
			}
			v, ok := c.Get(key)
			if ok != tc.wantMark || rec.StatusCode != tc.wantCode {
				t.Fatalf("marked=%v status=%d, want marked=%v status=%d", ok, rec.StatusCode, tc.wantMark, tc.wantCode)
			}
			if !ok {
				return
			}
			m, isRep := v.(interface {
				CompressibleRepresentation() (int, string)
			})
			if !isRep {
				t.Fatalf("the value under %q is %T, which does not answer CompressibleRepresentation", key, v)
			}
			if size, ct := m.CompressibleRepresentation(); size != len(body) || ct != "text/plain; charset=utf-8" {
				t.Errorf("mark = %d bytes of %q, want %d bytes of text/plain; charset=utf-8", size, ct, len(body))
			}
		})
	}
}
