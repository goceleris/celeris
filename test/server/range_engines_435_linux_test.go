//go:build linux

package celeris_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestFileRangeOnEveryEngine435 serves c.File over real connections on every
// engine, HTTP/1.1 and HTTP/2 (h2c), for celeris#435: a resume whose If-Range
// still matches gets its 206 (on epoll's worker that is sendfile(2): the file
// is past the 16 KiB threshold), a resume after the validator changed gets
// the whole file as a 200 instead of a 206 spliced onto the old copy, an
// unsatisfiable range gets 416 with "Content-Range: bytes */<size>" and an
// empty body framed so the connection stays usable, and HEAD ignores Range.
func TestFileRangeOnEveryEngine435(t *testing.T) {
	const size = 64 << 10
	body := bytes.Repeat([]byte("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ!?"), size/64)
	if len(body) != size {
		t.Fatalf("fixture is %d bytes", len(body))
	}
	path := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	const etag, lastMod = `"v2"`, "Wed, 01 Jul 2026 10:00:00 GMT"

	type req435 struct {
		name, method, rng, ifRange string
		status                     int
		crange                     string
		want                       []byte
	}
	reqs := []req435{
		{"resume-validator-matches", "GET", "bytes=1000-", etag, 206, "bytes 1000-65535/65536", body[1000:]},
		{"resume-date-matches", "GET", "bytes=1000-1999", lastMod, 206, "bytes 1000-1999/65536", body[1000:2000]},
		{"resume-after-change", "GET", "bytes=1000-", `"v1"`, 200, "", body},
		{"resume-after-change-date", "GET", "bytes=1000-", "Tue, 30 Jun 2026 10:00:00 GMT", 200, "", body},
		{"unsatisfiable", "GET", "bytes=70000-", "", 416, "bytes */65536", nil},
		{"after-416-same-conn", "GET", "", "", 200, "", body},
		// celeris#846 item 3: If-Range is checked before the range is parsed,
		// so a validator that no longer holds on a range that is
		// unsatisfiable gets the whole file (RFC 9110 13.2.2), not a 416.
		{"unsatisfiable-if-range-mismatch", "GET", "bytes=70000-", `"v1"`, 200, "", body},
		{"unsatisfiable-if-range-date-mismatch", "GET", "bytes=70000-", "Tue, 30 Jun 2026 10:00:00 GMT", 200, "", body},
		{"unsatisfiable-if-range-matches", "GET", "bytes=70000-", etag, 416, "bytes */65536", nil},
		{"after-if-range-416-same-conn", "GET", "", "", 200, "", body},
		{"head-ignores-range", "HEAD", "bytes=1000-", "", 200, "", nil},
		{"last-pos-past-end", "GET", "bytes=65000-99999", "", 206, "bytes 65000-65535/65536", body[65000:]},
	}

	for _, e := range engines761 {
		for _, route := range []string{"sync", "async-route"} {
			t.Run(e.name+"/"+route, func(t *testing.T) {
				addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
					serve := func(c *celeris.Context) error {
						c.SetHeader("etag", etag)
						c.SetHeader("last-modified", lastMod)
						return c.File(path)
					}
					// HEAD is registered explicitly: answering HEAD on a GET
					// route is celeris#421, not this test's subject.
					for _, r := range []*celeris.Route{s.GET("/f", serve), s.HEAD("/f", serve)} {
						if route == "async-route" {
							r.Async()
						}
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
						var dials atomic.Int32
						tr.DialContext = func(ctx context.Context, network, a string) (net.Conn, error) {
							dials.Add(1)
							var d net.Dialer
							return d.DialContext(ctx, network, a)
						}
						cl := &http.Client{Timeout: 10 * time.Second, Transport: tr}
						defer cl.CloseIdleConnections()
						for _, rq := range reqs {
							// Each request reports on its own (Errorf), so one run
							// shows every case a tree gets wrong; a transport error
							// ends the connection's sequence (Fatalf).
							desc := fmt.Sprintf("%s/%s/%s %s", e.name, route, proto, rq.name)
							hr, err := http.NewRequest(rq.method, "http://"+addr+"/f", nil)
							if err != nil {
								t.Fatal(err)
							}
							if rq.rng != "" {
								hr.Header.Set("Range", rq.rng)
							}
							if rq.ifRange != "" {
								hr.Header.Set("If-Range", rq.ifRange)
							}
							resp, err := cl.Do(hr)
							if err != nil {
								t.Fatalf("%s: %v", desc, err)
							}
							got, err := io.ReadAll(resp.Body)
							_ = resp.Body.Close()
							if err != nil {
								t.Fatalf("%s: reading the body: %v", desc, err)
							}
							if wantMajor := map[string]int{"h1": 1, "h2": 2}[proto]; resp.ProtoMajor != wantMajor {
								t.Fatalf("%s: answered over %s", desc, resp.Proto)
							}
							cr := resp.Header.Values("Content-Range")
							switch {
							case resp.StatusCode != rq.status || (rq.crange == "" && len(cr) != 0) ||
								(rq.crange != "" && (len(cr) != 1 || cr[0] != rq.crange)):
								t.Errorf("%s: status %d content-range %q (%d body bytes); want %d %q",
									desc, resp.StatusCode, cr, len(got), rq.status, rq.crange)
							case !bytes.Equal(got, rq.want):
								t.Errorf("%s: %d body bytes, want %d (equal=false)", desc, len(got), len(rq.want))
							case rq.method == "HEAD" && resp.ContentLength != size:
								t.Errorf("%s: HEAD content-length %d, want %d", desc, resp.ContentLength, size)
							}
						}
						if n := dials.Load(); n != 1 {
							t.Fatalf("%s/%s/%s: %d connections for %d requests, want 1 (a response's framing broke the connection)",
								e.name, route, proto, n, len(reqs))
						}
					})
				}
			})
		}
	}
}
