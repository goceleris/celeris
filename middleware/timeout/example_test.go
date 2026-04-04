package timeout_test

import (
	"time"

	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/timeout"
)

func ExampleNew() {
	// Zero-config: 5-second cooperative timeout.
	_ = timeout.New()
}

func ExampleNew_preemptive() {
	// Preemptive timeout: returns 503 even if handler is blocked.
	// Handlers MUST check c.Context().Done() for prompt cancellation.
	_ = timeout.New(timeout.Config{
		Timeout:    3 * time.Second,
		Preemptive: true,
	})
}

func ExampleNew_customErrorHandler() {
	// Custom error handler that receives the timeout cause.
	_ = timeout.New(timeout.Config{
		Timeout: 10 * time.Second,
		ErrorHandler: func(c *celeris.Context, err error) error {
			return c.JSON(503, map[string]string{"error": err.Error()})
		},
	})
}
