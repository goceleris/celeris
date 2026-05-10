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
// External property-test harnesses read the socket on every poll to
// feed property predicates. The counter shape is stable across both
// build modes via the [Counters] struct and the [Snapshot] function —
// the only thing that changes is whether reads return live atomics
// or the zero value.
//
// # Call-pattern convention
//
// Two call patterns coexist and SHOULD be used consistently:
//
//   - For unconditional event counts (panic recovered, etc.) call the
//     RecordX() helper defined in hooks.go (e.g. validation.RecordPanic()).
//     Helpers carry no build tag — they delegate to Counter.Add, which
//     inlines to nothing under !validation.
//   - For predicate-bearing checks (a value violates an invariant) call
//     the per-package validate*() helper defined in that package's
//     validation_check.go (under //go:build validation) with a no-op
//     stub in validation_default.go (under //go:build !validation).
//     Examples: middleware/jwt.validateAdmission,
//     middleware/ratelimit.validateBucket.
//
// Bare validation.Counter.Add(1) outside a validate*() helper is a
// bug — use a RecordX() helper instead.
package validation
