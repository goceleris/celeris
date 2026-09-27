//go:build !validation

package zcwindow

import "time"

// Enabled is false in production: the engine's call sites are guarded by
// it and compile away.
const Enabled = false

// SetHold is the production no-op: the celeris#587 window hold exists only
// under -tags=validation (zcwindow.go).
func SetHold(time.Duration) {}

// Hold is the production no-op.
func Hold() {}
