//go:build !validation

package main

import "log/slog"

// startValidationEndpoint is the production no-op: production
// binaries do NOT expose the unix socket and do NOT carry the
// assertion counters. Returns a no-op stop callback so main.go can
// defer it unconditionally.
func startValidationEndpoint(_ *slog.Logger) (func(), error) {
	return func() {}, nil
}
