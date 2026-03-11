package celeris

import (
	"context"
	"testing"

	"github.com/goceleris/celeris/protocol/h2/stream"
)

func BenchmarkRouterFind(b *testing.B) {
	r := newRouter()
	handler := func(_ *Context) error { return nil }

	r.addRoute("GET", "/", []HandlerFunc{handler})
	r.addRoute("GET", "/users", []HandlerFunc{handler})
	r.addRoute("GET", "/users/:id", []HandlerFunc{handler})
	r.addRoute("GET", "/users/:id/posts", []HandlerFunc{handler})
	r.addRoute("GET", "/users/:id/posts/:postId", []HandlerFunc{handler})
	r.addRoute("POST", "/users", []HandlerFunc{handler})
	r.addRoute("GET", "/static/*filepath", []HandlerFunc{handler})

	var params Params
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		params = params[:0]
		r.find("GET", "/users/123/posts/456", &params)
	}
}

func BenchmarkRouterFindStatic(b *testing.B) {
	r := newRouter()
	handler := func(_ *Context) error { return nil }

	r.addRoute("GET", "/users", []HandlerFunc{handler})

	var params Params
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		params = params[:0]
		r.find("GET", "/users", &params)
	}
}

func BenchmarkRouterFindCatchAll(b *testing.B) {
	r := newRouter()
	handler := func(_ *Context) error { return nil }

	r.addRoute("GET", "/static/*filepath", []HandlerFunc{handler})

	var params Params
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		params = params[:0]
		r.find("GET", "/static/css/style.css", &params)
	}
}

func BenchmarkContextAcquireRelease(b *testing.B) {
	s, _ := newTestStream("GET", "/")
	defer s.Release()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c := acquireContext(s)
		releaseContext(c)
	}
}

func BenchmarkContextJSON(b *testing.B) {
	s, rw := newTestStream("GET", "/")
	defer s.Release()

	data := map[string]string{"message": "hello"}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rw.status = 0
		rw.body = nil
		c := acquireContext(s)
		_ = c.JSON(200, data)
		releaseContext(c)
	}
}

func BenchmarkContextString(b *testing.B) {
	s, rw := newTestStream("GET", "/")
	defer s.Release()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rw.status = 0
		rw.body = nil
		c := acquireContext(s)
		_ = c.String(200, "Hello, World!")
		releaseContext(c)
	}
}

func BenchmarkContextBlob(b *testing.B) {
	s, rw := newTestStream("GET", "/")
	defer s.Release()

	payload := []byte("Hello, World!")

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rw.status = 0
		rw.body = nil
		c := acquireContext(s)
		_ = c.Blob(200, "text/plain", payload)
		releaseContext(c)
	}
}

func BenchmarkHandlerChain(b *testing.B) {
	s, rw := newTestStream("GET", "/")
	defer s.Release()

	middleware := func(c *Context) error {
		return c.Next()
	}
	handler := func(c *Context) error {
		return c.String(200, "ok")
	}

	chain := []HandlerFunc{middleware, middleware, middleware, handler}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rw.status = 0
		rw.body = nil
		c := acquireContext(s)
		c.handlers = chain
		c.index = -1
		_ = c.Next()
		releaseContext(c)
	}
}

func BenchmarkContextQuery(b *testing.B) {
	s, _ := newTestStream("GET", "/search?q=hello&page=1&limit=10")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = c.Query("q")
	}
}

func BenchmarkContextQueryFirstParse(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := stream.NewStream(1)
		s.Headers = [][2]string{
			{":method", "GET"},
			{":path", "/search?q=hello&page=1&limit=10"},
			{":scheme", "http"},
			{":authority", "localhost"},
		}
		rw := &mockResponseWriter{}
		s.ResponseWriter = rw

		c := acquireContext(s)
		_ = c.Query("q")
		releaseContext(c)
		s.Release()
	}
}

func BenchmarkContextSetGet(b *testing.B) {
	s, _ := newTestStream("GET", "/")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.Set("key", "value")
		_, _ = c.Get("key")
	}
}

func BenchmarkContextParam(b *testing.B) {
	s, _ := newTestStream("GET", "/users/42")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)
	c.params = Params{{Key: "id", Value: "42"}, {Key: "name", Value: "alice"}}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = c.Param("id")
		_ = c.Param("name")
	}
}

func BenchmarkContextSetHeader(b *testing.B) {
	s, _ := newTestStream("GET", "/")
	defer s.Release()

	c := acquireContext(s)
	defer releaseContext(c)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.respHeaders = c.respHeaders[:0]
		c.SetHeader("x-request-id", "abc-123")
		c.SetHeader("x-trace-id", "def-456")
	}
}

func BenchmarkHandleStreamFull(b *testing.B) {
	srv := New(Config{})
	srv.Use(func(c *Context) error {
		return c.Next()
	})
	srv.GET("/users/:id", func(c *Context) error {
		return c.String(200, "user-%s", c.Param("id"))
	})

	adapter := &routerAdapter{server: srv}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		st, _ := newTestStream("GET", "/users/42")
		_ = adapter.HandleStream(context.Background(), st)
		st.Release()
	}
}

func BenchmarkRouterFindManyRoutes(b *testing.B) {
	r := newRouter()
	handler := func(_ *Context) error { return nil }

	paths := []string{
		"/api/v1/users",
		"/api/v1/users/:id",
		"/api/v1/users/:id/posts",
		"/api/v1/users/:id/posts/:postId",
		"/api/v1/teams",
		"/api/v1/teams/:id",
		"/api/v1/teams/:id/members",
		"/api/v2/users",
		"/api/v2/users/:id",
		"/api/v2/products",
		"/api/v2/products/:id",
		"/api/v2/products/:id/reviews",
		"/health",
		"/ready",
		"/metrics",
		"/static/*filepath",
	}
	for _, p := range paths {
		r.addRoute("GET", p, []HandlerFunc{handler})
	}

	var params Params
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		params = params[:0]
		r.find("GET", "/api/v2/products/99/reviews", &params)
	}
}
