package adapters_test

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/adapters"
)

func ExampleReverseProxy_modifyResponse() {
	target, _ := url.Parse("http://backend:8080")

	s := celeris.New(celeris.Config{})
	s.Any("/api/*path", adapters.ReverseProxy(target,
		adapters.WithModifyResponse(func(resp *http.Response) error {
			resp.Header.Set("X-Proxy", "celeris")
			return nil
		}),
	))
	fmt.Println("proxy with response modifier configured")
	// Output: proxy with response modifier configured
}

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
