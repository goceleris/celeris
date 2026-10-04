//go:build linux && validation

package iouring

// zcValidationBuild reports whether this test binary carries the
// `validation` build tag, i.e. whether validation.Snapshot() reads live
// atomics rather than the no-op stubs in validation/disabled.go. The
// celeris#591 witness tests assert exact validation-counter deltas only
// when it is true; in a production build the same call sites must still
// compile and still be exercised, and the deltas must be 0.
const zcValidationBuild = true
