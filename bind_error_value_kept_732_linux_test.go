//go:build linux

package celeris_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/goceleris/celeris"
)

// TestBindErrorValueSurvivesNextRequest pins BindError.Value, the one error
// field of the celeris#732 class that celeris builds itself from a request
// string.
//
// Bind* reports a value that does not convert as a *BindError whose Value is
// the offending string. For a single value that was the request string
// itself (strings.Join returns one element as it is): on epoll and io_uring
// (and Adaptive, which runs them) a view of the connection's receive buffer,
// which the engine reuses for the connection's next request. The error
// outlives the request when a singleflight waiter or a cache follower
// reaches the leader's BindError through errors.As, or when an error
// reporter formats it later. Here the handler keeps every BindError, and one
// keep-alive connection sends three requests with the same layout and
// different values to a route that binds a path parameter and to one that
// binds the query. Once all six are answered, every kept Value must still
// read its own request.
func TestBindErrorValueSurvivesNextRequest(t *testing.T) {
	for _, a := range mw742Arms(t) {
		t.Run(a.name, func(t *testing.T) {
			var mu sync.Mutex
			var kept []*celeris.BindError
			keep := func(c *celeris.Context, err error) error {
				var be *celeris.BindError
				if !errors.As(err, &be) {
					return fmt.Errorf("bind returned %v, want a *BindError", err)
				}
				mu.Lock()
				kept = append(kept, be)
				mu.Unlock()
				return c.String(400, "bad")
			}
			addr, stop := startKeptServer(t, func() *celeris.Server {
				srv := celeris.New(celeris.Config{Engine: a.engine, AsyncHandlers: a.async})
				srv.GET("/p/:n", func(c *celeris.Context) error {
					var v struct {
						N int `param:"n"`
					}
					return keep(c, c.BindParams(&v))
				})
				srv.GET("/q", func(c *celeris.Context) error {
					var v struct {
						N int `query:"n"`
					}
					return keep(c, c.BindQuery(&v))
				})
				return srv
			})
			defer stop()

			conn, br := keptDial(t, addr)
			defer func() { _ = conn.Close() }()
			var want []string
			for _, route := range []string{"/p/%s", "/q?n=%s"} {
				for _, v := range []string{"aaaa", "bbbb", "cccc"} {
					mw742RoundTrip(t, conn, br, "GET "+fmt.Sprintf(route, v)+" HTTP/1.1\r\nHost: h\r\n\r\n", 400)
					want = append(want, v)
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if len(kept) != len(want) {
				t.Fatalf("kept %d BindErrors, want %d", len(kept), len(want))
			}
			var wrong []string
			for i, be := range kept {
				if be.Value != want[i] {
					wrong = append(wrong, fmt.Sprintf("request %d (%s %q): Value %q, want %q", i+1, be.Source, be.Key, be.Value, want[i]))
				}
			}
			t.Logf("MW732BIND arm=%s errors=%d wrong=%d", a.name, len(kept), len(wrong))
			if len(wrong) > 0 {
				t.Errorf("a kept BindError's Value reads other bytes after the connection's later requests:\n  %s", strings.Join(wrong, "\n  "))
			}
		})
	}
}
