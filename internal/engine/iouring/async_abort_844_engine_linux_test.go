//go:build linux

package iouring

import "testing"

// The io_uring half of the celeris#844 rig (async_abort_844_linux_test.go is
// the same file on both engines).

const (
	engineName844 = "io_uring"
	pkgDir844     = "iouring"
)

// tier844 names the recv tier the workers run: "high" with a provided buffer
// ring, "base" without (CELERIS_MAX_IOURING_TIER=base, or a kernel without
// it).
func tier844(e *Engine) string {
	e.mu.Lock()
	ws := e.workers
	e.mu.Unlock()
	for _, w := range ws {
		if w.bufRing != nil {
			return "high"
		}
	}
	return "base"
}

// parkRouteReachable844 reports whether the park loop of serveAsync reaches
// application code (the route resolver) under asyncInMu on this engine and
// tier: canRevertToInline short-circuits on w.bufRing == nil, so only the
// base tier calls RouteAsync there.
func parkRouteReachable844(e *Engine) (bool, string) {
	if tier := tier844(e); tier != "base" {
		return false, "tier " + tier + ": canRevertToInline never calls RouteAsync with a buffer ring, so no application code runs under asyncInMu"
	}
	return true, ""
}

func unavailable844(t *testing.T, format string, args ...any) {
	t.Helper()
	skipOrFail656(t, format, args...)
}
