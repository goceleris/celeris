//go:build linux

package celeris_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/cache"
	"github.com/goceleris/celeris/middleware/store"
)

// TestCacheHitContentTypeOnEveryEngine1156 drives celeris#1156 over real
// connections on every engine, HTTP/1.1 and HTTP/2 (h2c): a cache HIT carries
// the Content-Type its MISS carried (the handler's c.String, c.JSON and
// c.Blob types), where main answered every HIT with application/octet-stream.
func TestCacheHitContentTypeOnEveryEngine1156(t *testing.T) {
	routes := []struct {
		path, body, ct string
		handler        celeris.HandlerFunc
	}{
		{"/string", "hello", "text/plain", func(c *celeris.Context) error { return c.String(200, "hello") }},
		{"/json", `{"a":"b"}`, "application/json", func(c *celeris.Context) error { return c.JSON(200, map[string]string{"a": "b"}) }},
		{"/png", "PNG", "image/png", func(c *celeris.Context) error { return c.Blob(200, "image/png", []byte("PNG")) }},
	}
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			kv := store.NewMemoryKV()
			defer kv.Close()
			addr := startServer421(t, e.eng, func(s *celeris.Server) {
				s.Use(cache.New(cache.Config{Store: kv, TTL: time.Minute}))
				for _, r := range routes {
					s.GET(r.path, r.handler)
				}
			})
			for _, proto := range []string{"h1", "h2"} {
				t.Run(proto, func(t *testing.T) {
					tr := &http.Transport{MaxConnsPerHost: 1, DisableCompression: true}
					if proto == "h2" {
						p := new(http.Protocols)
						p.SetUnencryptedHTTP2(true)
						tr.Protocols = p
					}
					tr.DialContext = func(ctx context.Context, network, a string) (net.Conn, error) {
						var d net.Dialer
						return d.DialContext(ctx, network, a)
					}
					cl := &http.Client{Timeout: 10 * time.Second, Transport: tr}
					defer cl.CloseIdleConnections()
					for _, r := range routes {
						// The first request of a route is the MISS, the others are
						// HITs (the h2 pass sees HITs only): every response must
						// carry the handler's type.
						for i := range 3 {
							desc := fmt.Sprintf("%s/%s GET %s #%d", e.name, proto, r.path, i)
							resp, err := cl.Get("http://" + addr + r.path)
							if err != nil {
								t.Errorf("%s: %v", desc, err)
								continue
							}
							got, err := io.ReadAll(resp.Body)
							_ = resp.Body.Close()
							if err != nil {
								t.Errorf("%s: reading the body: %v", desc, err)
								continue
							}
							if resp.StatusCode != 200 || string(got) != r.body || resp.Header.Get("Content-Type") != r.ct {
								t.Errorf("%s: %d %q content-type %q x-cache %q, want 200 %q %q",
									desc, resp.StatusCode, got, resp.Header.Get("Content-Type"), resp.Header.Get("X-Cache"), r.body, r.ct)
							}
						}
					}
				})
			}
		})
	}
}
