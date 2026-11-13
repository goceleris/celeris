package celeris

import "testing"

var routeAsyncSink857 bool

// BenchmarkRouterAdapterRouteAsync857 is the per-request dispatch decision the H1
// engines take before a handler runs (internal/conn/h1.go). noquery is the
// common case and must stay allocation free; query is the request target of
// celeris#857, which used to miss the static map and walk the tree.
func BenchmarkRouterAdapterRouteAsync857(b *testing.B) {
	s := New(Config{})
	for _, p := range []string{"/", "/ping", "/json", "/plaintext", "/api/v1/health", "/api/v1/items"} {
		s.GET(p, noopHandler)
	}
	s.GET("/mc-multiget", noopHandler).Async()
	s.GET("/db/users", noopHandler).Async()
	s.GET("/users/:id", noopHandler)
	s.GET("/u/:id", noopHandler).Async()
	s.POST("/db/users", noopHandler).Async()
	a := &routerAdapter{server: s}
	for _, tc := range []struct{ name, method, target string }{
		{"static_async_noquery", "GET", "/mc-multiget"},
		{"static_sync_noquery", "GET", "/plaintext"},
		{"param_async_noquery", "GET", "/u/12345"},
		{"unmatched_noquery", "GET", "/nope"},
		{"static_async_query", "GET", "/mc-multiget?keys=10"},
		{"static_async_query_long", "GET", "/db/users?limit=50&offset=100&order=name&fields=id,name,email"},
		{"param_async_query", "GET", "/u/12345?limit=50"},
		{"unmatched_query", "GET", "/nope?a=1"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var r bool
			for i := 0; i < b.N; i++ {
				r = a.RouteAsync(tc.method, tc.target)
			}
			routeAsyncSink857 = r
		})
	}
}
