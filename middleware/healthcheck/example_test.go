package healthcheck_test

import (
	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/healthcheck"
)

func ExampleNew() {
	server := celeris.New(celeris.Config{})

	// All probes return 200 by default.
	server.Use(healthcheck.New())
}

func ExampleNew_customChecker() {
	server := celeris.New(celeris.Config{})

	server.Use(healthcheck.New(healthcheck.Config{
		ReadyChecker: func(_ *celeris.Context) bool {
			// Check database connectivity, etc.
			return true
		},
	}))
}

func ExampleNew_customPaths() {
	server := celeris.New(celeris.Config{})

	server.Use(healthcheck.New(healthcheck.Config{
		LivePath:  "/health/live",
		ReadyPath: "/health/ready",
		StartPath: "/health/startup",
	}))
}

func ExampleNew_fastPath() {
	server := celeris.New(celeris.Config{})

	server.Use(healthcheck.New(healthcheck.Config{
		CheckerTimeout: healthcheck.FastPathTimeout,
	}))
}
