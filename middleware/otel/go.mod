module github.com/goceleris/celeris/middleware/otel

go 1.27.2

require (
	github.com/goceleris/celeris v1.5.11
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/metric v1.47.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/otel/sdk/metric v1.47.0
	go.opentelemetry.io/otel/trace v1.47.0
	golang.org/x/net v0.61.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

// In the monorepo, build against the in-tree celeris core so submodule
// tests catch core breakage immediately. External consumers ignore this
// directive — they get the published version named in require above.
replace github.com/goceleris/celeris => ../../
