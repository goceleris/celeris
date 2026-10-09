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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/static"
)

// TestStaticPrecompressedRangeOnEveryEngine846 serves a pre-compressed variant
// through the static middleware (Root + Compress) over real connections on
// every engine, HTTP/1.1 and HTTP/2 (h2c), sync and async routes, all requests
// of a run on ONE connection (celeris#846). The variant is past the 16 KiB
// threshold, so on epoll's worker a 206 of it is sendfile(2).
//
// The variant is the representation the client holds: its Last-Modified and
// ETag are the variant's own, a range is cut from its bytes and counted in its
// size, a 416 names its size, and once only the variant is rebuilt (the
// original keeps its mtime) a resume with the old variant's validators
// restarts with the whole new variant instead of a 206 spliced onto the old
// head, and an If-None-Match of the old variant is not answered 304.
func TestStaticPrecompressedRangeOnEveryEngine846(t *testing.T) {
	const (
		origSize = 80 << 10
		gz1Size  = 48 << 10
		gz2Size  = 56 << 10
	)
	pat := func(n int, seed byte) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = 'a' + (seed+byte(i)+byte(i>>8))%26
		}
		return b
	}
	orig, gz1, gz2 := pat(origSize, 1), pat(gz1Size, 5), pat(gz2Size, 11)
	origTime := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	gz1Time := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	gz2Time := time.Date(2026, 7, 1, 11, 30, 0, 0, time.UTC)
	date := func(tm time.Time) string { return tm.Format(http.TimeFormat) }
	weak := func(tm time.Time, size int) string { return fmt.Sprintf(`W/"%x-%x"`, tm.Unix(), size) }

	for _, e := range engines761 {
		for _, route := range []string{"sync", "async-route"} {
			for _, proto := range []string{"h1", "h2"} {
				t.Run(e.name+"/"+route+"/"+proto, func(t *testing.T) {
					dir := t.TempDir()
					write := func(name string, data []byte, mt time.Time) {
						p := filepath.Join(dir, name)
						if err := os.WriteFile(p, data, 0o600); err != nil {
							t.Fatal(err)
						}
						if err := os.Chtimes(p, mt, mt); err != nil {
							t.Fatal(err)
						}
					}
					write("app.js", orig, origTime)
					write("app.js.gz", gz1, gz1Time)

					addr := startServer761(t, e.eng, false, func(s *celeris.Server) {
						// The middleware is the route's handler: Server.Use cannot be
						// called once startServer761 has registered /ping.
						serve := static.New(static.Config{Root: dir, Compress: true})
						for _, r := range []*celeris.Route{s.GET("/app.js", serve), s.HEAD("/app.js", serve)} {
							if route == "async-route" {
								r.Async()
							}
						}
					})

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

					var lm1, etag1 string
					type step struct {
						name, method string
						hdr          map[string]string
						status       int
						crange       string
						want         []byte
						ce           string // expected Content-Encoding ("" = none)
						vary         bool   // Vary names Accept-Encoding
						before       func()
						after        func(h http.Header)
					}
					gzip := func(kv ...string) map[string]string {
						m := map[string]string{"Accept-Encoding": "gzip"}
						for i := 0; i+1 < len(kv); i += 2 {
							m[kv[i]] = kv[i+1]
						}
						return m
					}
					steps := []step{
						{name: "variant", method: "GET", hdr: gzip(), status: 200, want: gz1, ce: "gzip", vary: true,
							after: func(h http.Header) {
								lm1, etag1 = h.Get("Last-Modified"), h.Get("Etag")
								if lm1 != date(gz1Time) || etag1 != weak(gz1Time, gz1Size) {
									t.Errorf("variant validators last-modified %q etag %q; want the variant's %q %q",
										lm1, etag1, date(gz1Time), weak(gz1Time, gz1Size))
								}
							}},
						{name: "resume-current", method: "GET", hdr: gzip("Range", "bytes=1000-", "If-Range", date(gz1Time)),
							status: 206, crange: fmt.Sprintf("bytes 1000-%d/%d", gz1Size-1, gz1Size), want: gz1[1000:], ce: "gzip", vary: true},
						{name: "unsatisfiable-for-the-variant", method: "GET", hdr: gzip("Range", "bytes=60000-", "If-Range", date(gz1Time)),
							status: 416, crange: fmt.Sprintf("bytes */%d", gz1Size)},
						{name: "after-416-same-conn", method: "GET", hdr: gzip(), status: 200, want: gz1, ce: "gzip", vary: true},
						{name: "not-modified", method: "GET", hdr: gzip("If-None-Match", weak(gz1Time, gz1Size)), status: 304, vary: true},
						{name: "identity-resume", method: "GET", hdr: map[string]string{"Range": "bytes=70000-", "If-Range": date(origTime)},
							status: 206, crange: fmt.Sprintf("bytes 70000-%d/%d", origSize-1, origSize), want: orig[70000:]},
						{name: "resume-after-rebuild", method: "GET", hdr: gzip("Range", "bytes=1000-", "If-Range", date(gz1Time)),
							status: 200, want: gz2, ce: "gzip", vary: true,
							before: func() { write("app.js.gz", gz2, gz2Time) }},
						{name: "unsatisfiable-after-rebuild-stale-if-range", method: "GET", hdr: gzip("Range", "bytes=999999-", "If-Range", date(gz1Time)),
							status: 200, want: gz2, ce: "gzip", vary: true},
						{name: "not-modified-of-the-old-variant", method: "GET", hdr: gzip("If-None-Match", weak(gz1Time, gz1Size)),
							status: 200, want: gz2, ce: "gzip", vary: true},
						{name: "range-of-the-new-variant", method: "GET", hdr: gzip("Range", "bytes=50000-"),
							status: 206, crange: fmt.Sprintf("bytes 50000-%d/%d", gz2Size-1, gz2Size), want: gz2[50000:], ce: "gzip", vary: true},
						{name: "unsatisfiable-for-the-new-variant", method: "GET", hdr: gzip("Range", "bytes=60000-"),
							status: 416, crange: fmt.Sprintf("bytes */%d", gz2Size)},
						{name: "head-ignores-range", method: "HEAD", hdr: gzip("Range", "bytes=1000-"), status: 200, ce: "gzip", vary: true},
						{name: "after-head-same-conn", method: "GET", hdr: gzip(), status: 200, want: gz2, ce: "gzip", vary: true},
					}
					for _, st := range steps {
						desc := fmt.Sprintf("%s/%s/%s %s", e.name, route, proto, st.name)
						if st.before != nil {
							st.before()
						}
						hr, err := http.NewRequest(st.method, "http://"+addr+"/app.js", nil)
						if err != nil {
							t.Fatal(err)
						}
						for k, v := range st.hdr {
							hr.Header.Set(k, v)
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
						hasVary := strings.Contains(strings.Join(resp.Header.Values("Vary"), ","), "Accept-Encoding")
						switch {
						case resp.StatusCode != st.status || (st.crange == "" && len(cr) != 0) ||
							(st.crange != "" && (len(cr) != 1 || cr[0] != st.crange)):
							t.Errorf("%s: status %d content-range %q (%d body bytes); want %d %q",
								desc, resp.StatusCode, cr, len(got), st.status, st.crange)
						case resp.Header.Get("Content-Encoding") != st.ce:
							t.Errorf("%s: content-encoding %q, want %q", desc, resp.Header.Get("Content-Encoding"), st.ce)
						case st.vary && !hasVary:
							t.Errorf("%s: no Vary: Accept-Encoding (%q)", desc, resp.Header.Values("Vary"))
						case st.method != "HEAD" && !bytes.Equal(got, st.want):
							t.Errorf("%s: %d body bytes, want %d (equal=false)", desc, len(got), len(st.want))
						case st.method == "HEAD" && (len(got) != 0 || resp.ContentLength != gz2Size):
							t.Errorf("%s: HEAD body %d bytes, content-length %d, want 0 and %d", desc, len(got), resp.ContentLength, gz2Size)
						}
						if st.after != nil {
							st.after(resp.Header)
						}
					}
					if n := dials.Load(); n != 1 {
						t.Fatalf("%s/%s/%s: %d connections for %d requests, want 1 (a response's framing broke the connection)",
							e.name, route, proto, n, len(steps))
					}
				})
			}
		}
	}
}
