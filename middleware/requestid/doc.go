// Package requestid provides request ID middleware for celeris.
//
// The middleware reads the request ID from the incoming X-Request-Id
// header (configurable). If the incoming ID is absent or invalid, a
// new UUID v4 is generated using a buffered random source that batches
// 256 UUIDs per crypto/rand syscall. The ID is set on both the response
// header and the context store under [ContextKey] ("request_id").
//
// Basic usage:
//
//	server.Use(requestid.New())
//
// Custom header and deterministic generator for testing:
//
//	server.Use(requestid.New(requestid.Config{
//	    Header:    "x-trace-id",
//	    Generator: requestid.CounterGenerator("req"),
//	}))
//
// # Input Validation
//
// Incoming IDs are validated: only printable ASCII (0x20-0x7E) is
// accepted, with a maximum length of 128 characters. Invalid or
// oversized IDs are silently replaced with a fresh UUID.
//
// # DisableTrustProxy
//
// [Config].DisableTrustProxy controls whether the inbound request header
// is accepted. When false (default), a valid inbound header is propagated
// as-is. When true, the inbound header is always ignored and a fresh ID
// is generated:
//
//	server.Use(requestid.New(requestid.Config{
//	    DisableTrustProxy: true,
//	}))
//
// # Retrieving the Request ID
//
// Use [FromContext] to retrieve the request ID from downstream handlers:
//
//	id := requestid.FromContext(c)
//
// Use [FromStdContext] to retrieve it from a stdlib [context.Context]:
//
//	id := requestid.FromStdContext(ctx)
//
// # Skipping
//
// Use [Config].Skip for dynamic skip logic or [Config].SkipPaths for
// path exclusions. SkipPaths uses exact path matching.
//
// # Custom Generator Validation
//
// Custom generator output is validated with the same rules as inbound
// headers. If the generator returns an invalid or empty string, it is
// retried up to 3 times before falling back to the built-in UUID.
//
// [CounterGenerator] returns a monotonic ID generator ("{prefix}-{N}")
// with zero syscalls after initialization.
//
// # Middleware Order
//
// Register first -- all downstream middleware can access the request ID.
package requestid
