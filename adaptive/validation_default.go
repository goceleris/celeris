//go:build linux && !validation

package adaptive

import "github.com/goceleris/celeris/engine"

// validateSwitch is the production no-op stub. Inlines to nothing
// — performSwitch pays zero overhead in production builds. The
// assertion-bearing implementation lives in validation_check.go and
// is compiled under -tags=validation.
//
//go:inline
func validateSwitch(_, _ engine.EngineMetrics) {}
