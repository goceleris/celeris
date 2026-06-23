// Package requestid provides request ID middleware for celeris.
//
// [New] reads the request ID from the incoming header (default: "x-request-id").
// If absent or invalid (non-printable ASCII, >128 bytes), a UUID v4 is generated
// using a buffered random source that batches 256 UUIDs per crypto/rand syscall.
// The ID is written to the response header and stored in the context under
// [ContextKey] ("request_id").
//
// Key exported symbols:
//   - [New] — constructor; accepts an optional [Config].
//   - [Config] — Header, Generator, DisableTrustProxy, EnableStdContext, Skip, SkipPaths.
//   - [FromContext] — retrieve the ID from a [celeris.Context] in downstream handlers.
//   - [FromStdContext] — retrieve the ID from a stdlib [context.Context] (requires Config.EnableStdContext).
//   - [CounterGenerator] — monotonic "{prefix}-{N}" generator; zero syscalls after init.
//   - [ContextKey] — the context-store key ("request_id").
//
// Register before logger so every log line carries the request ID.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs/observability#request-ids
package requestid
