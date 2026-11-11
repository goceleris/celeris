package celeris

import (
	"strings"
	"testing"
)

// celeris#889 (from #852's round-2 review): the 405's Allow header is set by
// the end of the 405 chain, not before the global middleware, so a
// middleware's own answer to a request whose method no route takes (an auth
// 401) does not tell an unauthenticated client which methods the path takes.

func TestAllowOnlyOnThe405Answer889(t *testing.T) {
	auth := func(c *Context) error {
		if c.Header("authorization") == "" {
			return c.Blob(401, "text/plain", []byte("unauthorized"))
		}
		return c.Next()
	}
	for _, custom := range []bool{false, true} {
		name := "builtin"
		if custom {
			name = "custom-handler"
		}
		t.Run(name, func(t *testing.T) {
			s := New(Config{})
			s.Use(auth)
			s.GET("/admin/users", func(c *Context) error { return c.String(200, "ok") })
			s.POST("/admin/users", func(c *Context) error { return c.String(201, "ok") })
			if custom {
				s.MethodNotAllowed(func(c *Context) error { return c.String(405, "custom 405") })
			}

			// Unauthenticated: the middleware answers, with no Allow, on a
			// path with routes and on one without.
			for _, rq := range [][2]string{{"DELETE", "/admin/users"}, {"GET", "/nope"}} {
				for arm, rw := range serve852(t, s, rq[0], rq[1]) {
					desc := arm + " " + rq[0] + " " + rq[1]
					r := one852(t, desc, rw)
					if r.status != 401 {
						t.Errorf("%s: status %d, want 401", desc, r.status)
					}
					if a := r.header("allow"); len(a) != 0 {
						t.Errorf("%s: Allow %v on the middleware's own 401: it names the methods the path takes to an unauthenticated client", desc, a)
					}
				}
			}

			// Admitted: the 405 itself carries the one Allow header listing
			// the methods the path takes, builtin or custom handler alike.
			s2 := New(Config{})
			s2.Use(func(c *Context) error { return c.Next() })
			s2.GET("/admin/users", func(c *Context) error { return c.String(200, "ok") })
			s2.POST("/admin/users", func(c *Context) error { return c.String(201, "ok") })
			if custom {
				s2.MethodNotAllowed(func(c *Context) error { return c.String(405, "custom 405") })
			}
			for arm, rw := range serve852(t, s2, "DELETE", "/admin/users") {
				r := one852(t, arm, rw)
				if r.status != 405 {
					t.Fatalf("%s: status %d, want 405", arm, r.status)
				}
				a := r.header("allow")
				if len(a) != 1 || !strings.Contains(a[0], "GET") || !strings.Contains(a[0], "POST") || strings.Contains(a[0], "DELETE") {
					t.Errorf("%s: Allow %v on the 405, want one header listing GET and POST and not DELETE", arm, a)
				}
			}
		})
	}
}

// TestAllowOnA405WithTheChainStartedByStart889: the chains Start builds
// (buildUnmatchedChains) end in the same Allow-setting step, and the context's
// per-request value does not leak into the next request that reuses it.
func TestAllowOnA405WithTheChainStartedByStart889(t *testing.T) {
	s := New(Config{})
	s.Use(func(c *Context) error {
		if c.Method() == "DELETE" && c.Header("authorization") == "" {
			return c.Blob(401, "text/plain", []byte("unauthorized"))
		}
		return c.Next()
	})
	s.GET("/x", func(c *Context) error { return c.String(200, "ok") })
	ra := &routerAdapter{server: s}
	ra.buildUnmatchedChains()
	run := func(method, path string, hdr ...string) recordedResponse852 {
		t.Helper()
		st, _ := newTestStream(method, path)
		for i := 0; i+1 < len(hdr); i += 2 {
			st.Headers = append(st.Headers, [2]string{hdr[i], hdr[i+1]})
		}
		rw := &recordingRW852{}
		st.ResponseWriter = rw
		if err := ra.HandleStream(t.Context(), st); err != nil {
			t.Fatal(err)
		}
		st.Release()
		return one852(t, method+" "+path, rw)
	}
	if r := run("DELETE", "/x"); r.status != 401 || len(r.header("allow")) != 0 {
		t.Errorf("unauthenticated DELETE /x: %d allow=%v, want 401 and no Allow", r.status, r.header("allow"))
	}
	if r := run("POST", "/x"); r.status != 405 || len(r.header("allow")) != 1 {
		t.Errorf("POST /x: %d allow=%v, want 405 with one Allow", r.status, r.header("allow"))
	}
	if r := run("GET", "/nope"); r.status != 404 || len(r.header("allow")) != 0 {
		t.Errorf("GET /nope: %d allow=%v, want 404 and no Allow", r.status, r.header("allow"))
	}
}
