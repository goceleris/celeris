package pprof_test

import (
	"os"

	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/pprof"
)

func ExampleNew() {
	server := celeris.New(celeris.Config{})

	// Default config: loopback-only access at /debug/pprof.
	server.Use(pprof.New())
}

func ExampleNew_customPrefix() {
	server := celeris.New(celeris.Config{})

	server.Use(pprof.New(pprof.Config{
		Prefix: "/profiling",
	}))
}

func ExampleNew_publicAccess() {
	server := celeris.New(celeris.Config{})

	server.Use(pprof.New(pprof.Config{
		AuthFunc: func(c *celeris.Context) bool {
			return c.Header("x-pprof-token") == os.Getenv("PPROF_TOKEN")
		},
	}))
}
