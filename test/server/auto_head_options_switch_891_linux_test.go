//go:build linux

package celeris_test

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/cors"
)

// TestDisableAutoHeadOptionsOnEveryEngine891 drives celeris#891 item 26 over
// real connections on every engine, HTTP/1.1 and HTTP/2 (h2c), all the
// requests of a run on one connection: with Config.DisableAutoHEAD a HEAD to
// a GET-only path is a 405 whose Allow does not list HEAD, with
// Config.DisableAutoOPTIONS an OPTIONS to a path with no OPTIONS route is a
// 405 whose Allow does not list OPTIONS and a CORS preflight installed with
// Server.Use is still answered, and explicit HEAD and OPTIONS routes answer in
// every combination. Each route runs once inline and once as an async route.
func TestDisableAutoHeadOptionsOnEveryEngine891(t *testing.T) {
	const origin = "https://a.example"
	type req struct {
		method, path string
		hdrs         map[string]string
		status       int
		allow        string // "-" = do not check
	}
	preflight := map[string]string{"Origin": origin, "Access-Control-Request-Method": "GET"}
	for _, sw := range []struct {
		name string
		cfg  celeris.Config
		reqs []req
	}{
		{"head", celeris.Config{DisableAutoHEAD: true}, []req{
			{"HEAD", "/hello", nil, 405, "GET, OPTIONS"},
			{"HEAD", "/eh", nil, 200, "-"},
			{"OPTIONS", "/hello", nil, 200, "GET, OPTIONS"},
			{"GET", "/hello", nil, 200, "-"},
			{"HEAD", "/nope", nil, 404, "-"},
		}},
		{"options", celeris.Config{DisableAutoOPTIONS: true}, []req{
			{"OPTIONS", "/hello", nil, 405, "GET, HEAD"},
			{"OPTIONS", "/hello", preflight, 204, "-"},
			{"OPTIONS", "/eo", nil, 204, "-"},
			{"HEAD", "/hello", nil, 200, "-"},
			{"GET", "/hello", nil, 200, "-"},
		}},
		{"both", celeris.Config{DisableAutoHEAD: true, DisableAutoOPTIONS: true}, []req{
			{"HEAD", "/hello", nil, 405, "GET"},
			{"OPTIONS", "/hello", nil, 405, "GET"},
			{"OPTIONS", "/hello", preflight, 204, "-"},
			{"HEAD", "/eh", nil, 200, "-"},
			{"OPTIONS", "/eo", nil, 204, "-"},
			{"GET", "/hello", nil, 200, "-"},
		}},
	} {
		for _, e := range engines761 {
			for _, route := range []string{"sync", "async-route"} {
				t.Run(sw.name+"/"+e.name+"/"+route, func(t *testing.T) {
					cfg := sw.cfg
					cfg.Engine = e.eng
					addr := startServerConfig421(t, cfg, func(s *celeris.Server) {
						s.Use(cors.New(cors.Config{AllowOrigins: []string{origin}}))
						routes := []*celeris.Route{
							s.GET("/hello", func(c *celeris.Context) error { return c.String(200, "hello world") }),
							s.GET("/eh", func(c *celeris.Context) error { return c.String(200, "get eh") }),
							s.HEAD("/eh", func(c *celeris.Context) error { return c.NoContent(200) }),
							s.GET("/eo", func(c *celeris.Context) error { return c.String(200, "get eo") }),
							s.OPTIONS("/eo", func(c *celeris.Context) error { return c.NoContent(204) }),
						}
						if route == "async-route" {
							for _, r := range routes {
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
							cl := &http.Client{Timeout: 10 * time.Second, Transport: tr}
							defer cl.CloseIdleConnections()
							wantMajor := map[string]int{"h1": 1, "h2": 2}[proto]
							for i, rq := range sw.reqs {
								desc := fmt.Sprintf("#%d %s %s", i, rq.method, rq.path)
								hr, err := http.NewRequest(rq.method, "http://"+addr+rq.path, nil)
								if err != nil {
									t.Fatal(err)
								}
								for k, v := range rq.hdrs {
									hr.Header.Set(k, v)
								}
								resp, err := cl.Do(hr)
								if err != nil {
									t.Errorf("%s: %v", desc, err)
									continue
								}
								_, _ = io.Copy(io.Discard, resp.Body)
								_ = resp.Body.Close()
								allow := strings.Join(resp.Header.Values("Allow"), "|")
								switch {
								case resp.ProtoMajor != wantMajor:
									t.Errorf("%s: answered over %s", desc, resp.Proto)
								case resp.StatusCode != rq.status:
									t.Errorf("%s: %d, want %d", desc, resp.StatusCode, rq.status)
								case rq.allow != "-" && allow != rq.allow:
									t.Errorf("%s: Allow %q, want %q", desc, allow, rq.allow)
								}
							}
						})
					}
				})
			}
		}
	}
}
