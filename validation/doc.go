// Package validation exposes in-process assertion counters that the
// celeris engine and middleware bump under the `validation` build tag.
//
// Production binaries (built without the tag) compile against the
// no-op stubs in disabled.go, so the counters and the unix-socket
// endpoint are stripped at compile time — no allocations, no atomic
// adds, no goroutines. Validation builds (-tags=validation) compile
// against assertions.go and endpoint.go, exposing named [Counter]
// variables (e.g. [PanicCount], [RatelimitTokenViolations]) as
// atomic.Uint64 and serving a JSON snapshot over [SocketPath] via
// [StartEndpoint].
//
// [Snapshot] returns a [Counters] struct whose shape is stable across
// both build modes — reads return live atomics under -tags=validation
// and zero values otherwise. External property-test harnesses poll
// [Snapshot] or read [SocketPath] on every tick to feed predicates.
//
// Unconditional event counts (e.g. recovered panics) are recorded via
// thin helpers such as [RecordPanic]; these carry no build tag and
// inline to nothing in production builds.
//
// # Documentation
//
// Full guides and examples: https://goceleris.dev/docs
package validation
