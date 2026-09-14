//go:build linux && !validation

package websocket

// wsZCValidationBuild is false in a production build: validation.Snapshot()
// returns the zero Counters and every validation.X.Add is a no-op. See the
// `validation` variant of this constant.
const wsZCValidationBuild = false
