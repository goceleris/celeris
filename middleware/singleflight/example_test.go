package singleflight_test

import (
	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/singleflight"
)

func ExampleNew() {
	// Deduplicate concurrent identical GET requests using the default key
	// (method + path + sorted query string).
	// s := celeris.New()
	// s.Use(singleflight.New())
	_ = singleflight.New()
}

func ExampleNew_customKey() {
	// Use only the path as the deduplication key, ignoring query parameters.
	_ = singleflight.New(singleflight.Config{
		KeyFunc: func(c *celeris.Context) string {
			return c.Method() + "\x00" + c.Path()
		},
	})
}
