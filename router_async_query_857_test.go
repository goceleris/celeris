package celeris

import "testing"

// celeris#857: the H1 engines hand RouteAsync the request target as it came off
// the wire, query string included (internal/conn/h1.go: state.req.Path), while
// the router is keyed by path alone and Context.extractRequestInfo cuts the
// target at the first '?'. routerAdapter.RouteAsync must cut it the same way, or
// an Async route requested with a query string is never promoted to the
// dispatch goroutine.
func TestRouterAdapterRouteAsyncIgnoresQuery857(t *testing.T) {
	s := New(Config{})
	s.GET("/x", noopHandler).Async()
	s.GET("/u/:id", noopHandler).Async()
	s.GET("/sync", noopHandler)
	s.GET("/only-get", noopHandler).Async()
	s.Handle("PURGE", "/cache", noopHandler).Async()
	a := &routerAdapter{server: s}

	for _, tc := range []struct {
		name, method, target string
		want                 bool
	}{
		{"static no query", "GET", "/x", true},
		{"static with query", "GET", "/x?keys=10", true},
		{"static empty query", "GET", "/x?", true},
		{"static query holding a slash", "GET", "/x?next=/sync&a=b", true},
		{"static query holding a second ?", "GET", "/x?a=?b", true},
		{"param no query", "GET", "/u/7", true},
		{"param with query", "GET", "/u/7?limit=50", true},
		{"param with a slash in the query", "GET", "/u/7?next=/a/b", true},
		{"sync with query", "GET", "/sync?a=1", false},
		{"sync no query", "GET", "/sync", false},
		{"unmatched no query", "GET", "/missing", false},
		{"unmatched with query", "GET", "/missing?a=1", false},
		{"query only names another route", "GET", "/sync?x=/x", false},
		{"HEAD as GET no query", "HEAD", "/only-get", true},
		{"HEAD as GET with query", "HEAD", "/only-get?a=1", true},
		{"HEAD as GET sync with query", "HEAD", "/sync?a=1", false},
		{"custom method no query", "PURGE", "/cache", true},
		{"custom method with query", "PURGE", "/cache?a=1", true},
		{"wrong method with query", "POST", "/x?a=1", false},
	} {
		if got := a.RouteAsync(tc.method, tc.target); got != tc.want {
			t.Errorf("%s: RouteAsync(%q, %q) = %v, want %v", tc.name, tc.method, tc.target, got, tc.want)
		}
	}
}

// An adaptive route (server default AsyncHandlers) that has been promoted is
// found through its query string too: the adaptive key is the route pattern.
func TestRouterAdapterRouteAsyncAdaptiveIgnoresQuery857(t *testing.T) {
	s := New(Config{AsyncHandlers: true})
	s.GET("/ad", noopHandler)
	a := &routerAdapter{server: s}
	if a.RouteAsync("GET", "/ad?a=1") {
		t.Fatal("an adaptive route starts inline, with or without a query")
	}
	s.router.promoteRoute("/ad")
	if !a.RouteAsync("GET", "/ad") || !a.RouteAsync("GET", "/ad?a=1") {
		t.Fatal("a promoted adaptive route must resolve async with and without a query")
	}
}
