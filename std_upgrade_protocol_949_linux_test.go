//go:build linux

package celeris_test

import (
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// celeris#949, round-3 review: the upgrade request of an h2c connection is
// HTTP/2 stream 1, so its handler must see c.Protocol() == "2" on every engine
// (for the std engine, under any build tag). With -tags http2legacy, x/net's
// legacy server passed the upgrade request on as HTTP/1 (ProtoMajor 1), so the
// std bridge took its HTTP/1 path: c.Protocol() reported "1.1", the request's
// context was never tied to the stream, and the drain gate was skipped. The
// fix marks the request as HTTP/2 before it is handed to ServeConn (h2c.go).
func TestUpgradeRequestIsHTTP2949(t *testing.T) {
	for _, e := range engines761 {
		for _, sh := range shapes949 {
			t.Run(e.name+"/"+sh.name, func(t *testing.T) {
				protos := make(chan string, 1)
				addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
					s.GET("/wait", func(c *celeris.Context) error {
						select {
						case protos <- c.Protocol():
						default:
						}
						return c.String(200, "done")
					})
				})
				r := sh.open(t, addr)
				select {
				case p := <-protos:
					if p != "2" {
						t.Fatalf("%s/%s: the handler's c.Protocol() = %q, want \"2\" (the request is HTTP/2 stream 1)", e.name, sh.name, p)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("%s/%s: the handler never ran", e.name, sh.name)
				}
				select {
				case st := <-r.status:
					if st != "200" {
						t.Fatalf("%s/%s: answered :status %q, want 200", e.name, sh.name, st)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("%s/%s: the client got no response", e.name, sh.name)
				}
			})
		}
	}
}
