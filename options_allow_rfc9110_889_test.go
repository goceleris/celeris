package celeris

import "testing"

// celeris#889 (NEW-1 of the umbrella triage): the Allow header of an OPTIONS
// request the router answers is set before the global middleware runs, so a
// middleware's own answer to such a request (an authentication 401, a CORS
// preflight's 204) carries the method list of a routed path too. This is
// intended, and the tests pin it with the RFC reading that makes it so:
//
//   - RFC 9110 §10.2.1: an origin server MUST generate Allow in a 405 and MAY
//     do so in any other response. Allow on a 401, a 204 or a 200 contradicts
//     nothing; and §9.3.7 has the server send Allow in its success answer to
//     OPTIONS, which is the answer a middleware that lets the request through
//     (or a preflight handler that answers it) stands in for.
//   - The 405 is the other way round (celeris#889, allow_on_405_answer_889_test.go):
//     its Allow is set at the end of the chain, since a 405 is a statement
//     about the method and a 401 to an unauthenticated DELETE must not
//     list what the path takes. An OPTIONS request asks exactly that question.
//
// An application that must not name a path's methods to an unauthenticated
// client sets Config.DisableAutoOPTIONS and answers OPTIONS in its own
// middleware or routes.
func TestAllowOnTheMiddlewaresOwnAnswerToOPTIONS889(t *testing.T) {
	const want = "GET, POST, HEAD, OPTIONS"
	newServer := func(disable bool) *Server {
		s := New(Config{DisableAutoOPTIONS: disable})
		s.Use(func(c *Context) error {
			if c.Header("authorization") == "" {
				return c.Blob(401, "text/plain", []byte("unauthorized"))
			}
			if c.Method() == "OPTIONS" && c.Header("access-control-request-method") != "" {
				return c.NoContent(204)
			}
			return c.Next()
		})
		s.GET("/admin/users", func(c *Context) error { return c.String(200, "ok") })
		s.POST("/admin/users", func(c *Context) error { return c.String(201, "ok") })
		return s
	}

	t.Run("401-carries-the-routed-path-Allow", func(t *testing.T) {
		r := serve421(t, newServer(false), "OPTIONS", "/admin/users")
		if r.status != 401 || r.header("allow") != want || r.headerCount("allow") != 1 {
			t.Fatalf("unauthenticated OPTIONS: %d, allow %q (x%d), want 401 with one Allow %q",
				r.status, r.header("allow"), r.headerCount("allow"), want)
		}
	})
	t.Run("preflight-204-carries-it", func(t *testing.T) {
		r := serve421(t, newServer(false), "OPTIONS", "/admin/users",
			[2]string{"authorization", "x"}, [2]string{"access-control-request-method", "GET"})
		if r.status != 204 || r.header("allow") != want {
			t.Fatalf("preflight: %d, allow %q, want 204 with Allow %q", r.status, r.header("allow"), want)
		}
	})
	t.Run("a-path-with-no-route-has-no-Allow", func(t *testing.T) {
		r := serve421(t, newServer(false), "OPTIONS", "/nope")
		if r.status != 401 || r.header("allow") != "" {
			t.Fatalf("OPTIONS on an unrouted path: %d, allow %q, want 401 and none", r.status, r.header("allow"))
		}
	})
	t.Run("DisableAutoOPTIONS-leaves-it-to-the-application", func(t *testing.T) {
		r := serve421(t, newServer(true), "OPTIONS", "/admin/users")
		if r.status != 401 || r.header("allow") != "" {
			t.Fatalf("with DisableAutoOPTIONS: %d, allow %q, want 401 and no Allow", r.status, r.header("allow"))
		}
	})
	t.Run("405-still-sets-Allow-at-the-end-of-the-chain", func(t *testing.T) {
		r := serve421(t, newServer(false), "DELETE", "/admin/users")
		if r.status != 401 || r.header("allow") != "" {
			t.Fatalf("unauthenticated DELETE: %d, allow %q, want the middleware's 401 and no Allow", r.status, r.header("allow"))
		}
		r = serve421(t, newServer(false), "DELETE", "/admin/users", [2]string{"authorization", "x"})
		if r.status != 405 || r.header("allow") != want {
			t.Fatalf("authenticated DELETE: %d, allow %q, want 405 with Allow %q", r.status, r.header("allow"), want)
		}
	})
}
