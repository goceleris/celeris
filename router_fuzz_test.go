package celeris

import (
	"strings"
	"testing"
)

func FuzzCleanPath(f *testing.F) {
	f.Add("/")
	f.Add("/a")
	f.Add("//a")
	f.Add("///a///b///")
	f.Add("//")
	f.Add("/a/b/c")
	f.Add("/a//b")
	f.Fuzz(func(t *testing.T, path string) {
		// cleanPath is only called on paths from HTTP requests, which
		// always start with '/'. Prefix to ensure valid input.
		if len(path) == 0 || path[0] != '/' {
			path = "/" + path
		}
		result := cleanPath(path)
		if len(result) == 0 || result[0] != '/' {
			t.Errorf("cleanPath(%q) = %q, must start with /", path, result)
		}
		if len(result) > 1 && strings.Contains(result, "//") {
			t.Errorf("cleanPath(%q) = %q, contains //", path, result)
		}
	})
}

func FuzzRouterFind(f *testing.F) {
	f.Add("/")
	f.Add("/users/42")
	f.Add("/files/css/style.css")
	f.Add("/api/v1/items")
	f.Add("")
	f.Add("//")
	f.Add("/a/b/c/d/e/f")
	f.Add("/users/")
	f.Add("/%00/null")
	f.Fuzz(func(_ *testing.T, path string) {
		r := newRouter()
		noop := []HandlerFunc{func(_ *Context) error { return nil }}
		r.addRoute("GET", "/", noop)
		r.addRoute("GET", "/users/:id", noop)
		r.addRoute("GET", "/files/*filepath", noop)
		r.addRoute("GET", "/api/v1/items", noop)
		var params Params
		r.find("GET", path, &params) // must not panic
	})
}
