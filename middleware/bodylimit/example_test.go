package bodylimit_test

import (
	"github.com/goceleris/celeris/middleware/bodylimit"
)

func ExampleNew() {
	// Zero-config: 4 MB limit.
	_ = bodylimit.New()
}

func ExampleNew_humanReadable() {
	// Human-readable size limit.
	_ = bodylimit.New(bodylimit.Config{
		Limit: "10MB",
	})
}
