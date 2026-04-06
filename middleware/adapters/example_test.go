package adapters_test

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/adapters"
)

func ExampleWrapMiddleware() {
	// Wrap a standard net/http middleware that adds a header.
	addHeader := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Middleware", "active")
			next.ServeHTTP(w, r)
		})
	}

	s := celeris.New(celeris.Config{})
	s.Use(adapters.WrapMiddleware(addHeader))
	s.GET("/", func(c *celeris.Context) error {
		return c.String(200, "ok")
	})
	fmt.Println("middleware registered")
	// Output: middleware registered
}

func ExampleToStdlib() {
	h := func(c *celeris.Context) error {
		return c.String(200, "hello from celeris")
	}

	// Use a celeris handler in a net/http server.
	_ = adapters.ToStdlib(h)
	fmt.Println("handler converted")
	// Output: handler converted
}

func ExampleReverseProxy() {
	target, _ := url.Parse("http://backend:8080")

	s := celeris.New(celeris.Config{})
	s.Any("/api/*path", adapters.ReverseProxy(target,
		adapters.WithModifyRequest(func(r *http.Request) {
			r.Header.Set("X-Forwarded-By", "celeris")
		}),
	))
	fmt.Println("proxy configured")
	// Output: proxy configured
}
