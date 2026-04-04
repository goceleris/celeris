package requestid_test

import (
	"github.com/goceleris/celeris/middleware/requestid"
)

func ExampleNew() {
	// Zero-config: generates UUID v4, propagates existing x-request-id.
	_ = requestid.New()
}

func ExampleNew_counter() {
	// Deterministic counter generator for testing.
	_ = requestid.New(requestid.Config{
		Generator: requestid.CounterGenerator("req"),
	})
}
