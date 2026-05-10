package validation

// RecordPanic increments the recovered-panic counter. Defined without
// a build tag so callers reach the production no-op (Counter.Add is a
// zero-cost stub) under regular builds and the live atomic under
// -tags=validation.
//
// Call-pattern convention:
//   - Bare Counter.Add(1) is reserved for predicate-bearing
//     validate*() helpers in the validation_check.go files (each
//     middleware/engine package owns its own helper). Those helpers
//     are split across build tags so the production binary inlines
//     to nothing.
//   - Unconditional event counts (panic recovered, etc.) go through
//     a thin RecordX() helper in this file. Keeping the convention
//     consistent makes the call-site grep a one-liner: any direct
//     Counter.Add outside a validate*() function is a bug.
func RecordPanic() { PanicCount.Add(1) }
