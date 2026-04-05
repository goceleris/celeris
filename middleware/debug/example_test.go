package debug_test

import (
	"os"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/observe"

	"github.com/goceleris/celeris/middleware/debug"
)

func ExampleNew() {
	server := celeris.New(celeris.Config{})
	collector := observe.NewCollector()

	// Default config: localhost-only access.
	server.Use(debug.New(debug.Config{
		Server:    server,
		Collector: collector,
	}))
}

func ExampleNew_withAuth() {
	server := celeris.New(celeris.Config{})
	collector := observe.NewCollector()

	server.Use(debug.New(debug.Config{
		Server:    server,
		Collector: collector,
		AuthFunc: func(c *celeris.Context) bool {
			return c.Header("x-debug-token") == os.Getenv("DEBUG_TOKEN")
		},
	}))
}

func ExampleNew_publicAccess() {
	server := celeris.New(celeris.Config{})

	server.Use(debug.New(debug.Config{
		Server:   server,
		AuthFunc: func(*celeris.Context) bool { return true },
	}))
}

func ExampleNew_customPrefix() {
	server := celeris.New(celeris.Config{})

	server.Use(debug.New(debug.Config{
		Server: server,
		Prefix: "/_internal",
	}))
}
