//go:build linux && validation

package websocket

// wsZCValidationBuild reports whether this test binary carries the
// `validation` build tag, i.e. whether validation.Snapshot() reads live
// atomics rather than the no-op stubs in validation/disabled.go. The
// celeris#591 rig asserts exact validation-counter deltas only when it is
// true; in a production build the same call sites still run and every delta
// must be 0.
const wsZCValidationBuild = true
