package ratelimit_test

import (
	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/ratelimit"
)

func ExampleNew() {
	// Zero-config: 10 RPS, burst 20, keyed by client IP.
	_ = ratelimit.New()
}

func ExampleNew_humanReadable() {
	// Human-readable rate: 100 requests per minute.
	_ = ratelimit.New(ratelimit.Config{
		Rate: "100-M",
	})
}

func ExampleNew_apiKey() {
	// Rate limit by API key header.
	_ = ratelimit.New(ratelimit.Config{
		Rate: "1000-H",
		KeyFunc: func(c *celeris.Context) string {
			return c.Header("x-api-key")
		},
	})
}

func ExampleParseRate() {
	rps, burst, _ := ratelimit.ParseRate("100-M")
	_ = rps   // 1.6667 (100/60)
	_ = burst // 100
}
