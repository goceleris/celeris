//go:build linux && !validation

package iouring

// zcValidationBuild is false in a production build: validation.Snapshot()
// returns the zero Counters and every validation.X.Add is a no-op. See
// the doc on the `validation` variant of this constant.
const zcValidationBuild = false
