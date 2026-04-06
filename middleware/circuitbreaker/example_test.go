package circuitbreaker_test

import (
	"fmt"
	"time"

	"github.com/goceleris/celeris/middleware/circuitbreaker"
)

func ExampleNew() {
	// Zero-config: 50% threshold, 10 min requests, 10s window, 30s cooldown.
	_ = circuitbreaker.New()
}

func ExampleNew_custom() {
	// Custom threshold and cooldown period.
	_ = circuitbreaker.New(circuitbreaker.Config{
		Threshold:      0.3,
		MinRequests:    20,
		CooldownPeriod: time.Minute,
	})
}

func ExampleNew_perGroup() {
	// Separate circuit breakers for different upstream services.
	// s := celeris.New()
	// payments := s.Group("/api/payments")
	// payments.Use(circuitbreaker.New(circuitbreaker.Config{Threshold: 0.3}))
	// internal := s.Group("/internal")
	// internal.Use(circuitbreaker.New(circuitbreaker.Config{Threshold: 0.7}))
	_ = circuitbreaker.New(circuitbreaker.Config{Threshold: 0.3})
}

func ExampleNewWithBreaker() {
	// Obtain a Breaker reference for programmatic state inspection.
	mw, breaker := circuitbreaker.NewWithBreaker(circuitbreaker.Config{
		Threshold:   0.5,
		MinRequests: 10,
		OnStateChange: func(from, to circuitbreaker.State) {
			fmt.Printf("circuit breaker: %s -> %s\n", from, to)
		},
	})
	_ = mw

	// Use the breaker for health checks or admin endpoints.
	fmt.Println(breaker.State())

	// Output:
	// closed
}
