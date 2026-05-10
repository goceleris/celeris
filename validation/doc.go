// Package validation exposes in-process assertion counters that the
// celeris engine and middleware bump under the `validation` build tag.
//
// Production binaries (built without the tag) compile against the
// no-op stubs in disabled.go, so the counters and the unix-socket
// endpoint are stripped at compile time — no allocations, no atomic
// adds, no goroutines. Validation builds (-tags=validation) compile
// against assertions.go and endpoint.go, exposing the counters as
// atomic.Uint64 and serving a JSON snapshot over
// /tmp/celeris-validation.sock.
//
// External property-test harnesses (probatorium's
// cmd/validator-checker) read the socket on every poll to feed
// property predicates. The counter shape is stable across both build
// modes via the [Counters] struct and the [Snapshot] function — the
// only thing that changes is whether reads return live atomics or
// the zero value.
package validation
