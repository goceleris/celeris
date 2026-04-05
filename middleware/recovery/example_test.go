package recovery_test

import (
	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/recovery"
)

func ExampleNew() {
	// Zero-config: catches panics, logs stack trace, returns 500 JSON.
	_ = recovery.New()
}

func ExampleNew_customHandler() {
	// Custom error handler for panics.
	_ = recovery.New(recovery.Config{
		ErrorHandler: func(c *celeris.Context, err any) error {
			return c.String(500, "something went wrong: %v", err)
		},
	})
}

func ExampleNew_stackAll() {
	// Capture all goroutine stacks (not just the panicking one).
	_ = recovery.New(recovery.Config{
		StackAll:  true,
		StackSize: 8192,
	})
}
