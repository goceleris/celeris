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
	"github.com/goceleris/celeris/middleware/singleflight"
)

// TestBufferedBlobWithEmptyContentTypeOnEveryEngine1155 drives celeris#1155
// over real connections on every engine, HTTP/1.1 and HTTP/2 (h2c): a body
// written without a Content-Type (c.Blob(200, "", ...), which is what Adapt
// writes for a net/http handler that sets none) reaches the client when a
// middleware buffered the response (c.BufferResponse / c.FlushResponse, and
// singleflight, which does so for its leader). On main the client got
// 200 with content-length: 0.
func TestBufferedBlobWithEmptyContentTypeOnEveryEngine1155(t *testing.T) {
	// buffer is the smallest middleware that buffers and flushes.
	buffer := func(c *celeris.Context) error {
		c.BufferResponse()
		err := c.Next()
		if ferr := c.FlushResponse(); ferr != nil && err == nil {
			err = ferr
		}
		return err
	}
	adapted := celeris.AdaptFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hi")) // no Content-Type
	})
	blob := func(c *celeris.Context) error { return c.Blob(200, "", []byte("hi")) }

	type route struct {
		path    string
		handler []celeris.HandlerFunc
	}
	routes := []route{
		{"/adapt-buffer", []celeris.HandlerFunc{buffer, adapted}},
		{"/adapt-singleflight", []celeris.HandlerFunc{singleflight.New(), adapted}},
		{"/blob-buffer", []celeris.HandlerFunc{buffer, blob}},
		{"/blob-singleflight", []celeris.HandlerFunc{singleflight.New(), blob}},
		{"/adapt-plain", []celeris.HandlerFunc{adapted}},
	}

	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := startServer421(t, e.eng, func(s *celeris.Server) {
				for _, r := range routes {
					s.GET(r.path, r.handler...)
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
						// twice: the second request reuses the connection, so a
						// framing error of the first shows up here too.
						for i := range 2 {
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
							if resp.StatusCode != 200 || string(got) != "hi" {
								t.Errorf("%s: %d %q (content-length %d), want 200 \"hi\"", desc, resp.StatusCode, got, resp.ContentLength)
							}
						}
					}
				})
			}
		})
	}
}
