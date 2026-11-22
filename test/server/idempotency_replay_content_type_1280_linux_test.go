//go:build linux

package celeris_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/idempotency"
)

// TestIdempotencyReplayContentTypeOnEveryEngine1280 drives celeris#1280 over real
// connections on every engine, HTTP/1.1 and HTTP/2 (h2c): a replayed response
// (the same Idempotency-Key again) carries the Content-Type the first response
// carried (the handler's c.String, c.JSON and c.Blob types) and a replayed 204
// stays a 204 with no type and no body, where main answered every replay with
// application/octet-stream (a 204 as a Blob with content-length 0).
func TestIdempotencyReplayContentTypeOnEveryEngine1280(t *testing.T) {
	routes := []struct {
		path, body, ct string
		status         int
		handler        celeris.HandlerFunc
	}{
		{"/string", "hello", "text/plain", 201, func(c *celeris.Context) error { return c.String(201, "hello") }},
		{"/json", `{"a":"b"}`, "application/json", 201, func(c *celeris.Context) error { return c.JSON(201, map[string]string{"a": "b"}) }},
		{"/png", "PNG", "image/png", 200, func(c *celeris.Context) error { return c.Blob(200, "image/png", []byte("PNG")) }},
		{"/raw", "raw", "application/octet-stream", 200, func(c *celeris.Context) error {
			return c.Blob(200, "application/octet-stream", []byte("raw"))
		}},
		{"/gone", "", "", 204, func(c *celeris.Context) error { return c.NoContent(204) }},
	}
	for _, e := range engines761 {
		t.Run(e.name, func(t *testing.T) {
			addr := startServer421(t, e.eng, func(s *celeris.Server) {
				s.Use(idempotency.New(idempotency.Config{TTL: time.Minute}))
				for _, r := range routes {
					s.POST(r.path, r.handler)
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
						// The h1 pass sends a route's first request, the others are
						// replays (the h2 pass sees replays only): every response must
						// carry the type, and the length, of the first.
						key := fmt.Sprintf("%s-%s", e.name, strings.TrimPrefix(r.path, "/"))
						for i := range 3 {
							desc := fmt.Sprintf("%s/%s POST %s #%d", e.name, proto, r.path, i)
							req, err := http.NewRequest("POST", "http://"+addr+r.path, strings.NewReader(""))
							if err != nil {
								t.Fatal(err)
							}
							req.Header.Set("Idempotency-Key", key)
							resp, err := cl.Do(req)
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
							_, hasCT := resp.Header["Content-Type"]
							if resp.StatusCode != r.status || string(got) != r.body || resp.Header.Get("Content-Type") != r.ct || (r.ct == "") == hasCT {
								t.Errorf("%s: %d %q content-type %q (present %v), want %d %q %q",
									desc, resp.StatusCode, got, resp.Header.Get("Content-Type"), hasCT, r.status, r.body, r.ct)
							}
							// A 204 has no content-length at all; the others have the body's.
							wantCL := fmt.Sprint(len(r.body))
							if r.status == 204 {
								wantCL = ""
							}
							if gotCL := resp.Header.Get("Content-Length"); gotCL != wantCL {
								t.Errorf("%s: content-length %q, want %q", desc, gotCL, wantCL)
							}
						}
					}
				})
			}
		})
	}
}
