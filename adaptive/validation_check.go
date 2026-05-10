//go:build linux && validation

package adaptive

import (
	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/validation"
)

// validateSwitch is invoked at the switch boundary in performSwitch
// with the engine-metrics snapshots taken immediately before
// ResumeAccept on the new active and immediately after PauseAccept
// on the old active. The wave-7 invariant is:
//
//	pre.ActiveConnections ⊆ post.ActiveConnections ∪ closed-during-switch
//
// Without per-FD identity to enumerate the actual sets we approximate
// the invariant numerically: a switch must not orphan more
// connections than legitimately closed during the swap. The
// "legitimate" close budget is bounded by the request count delta —
// if zero new requests landed during the swap, no in-flight close
// should be possible either. Conversely, a positive RequestCount
// delta admits up to that many close events.
//
// We increment validation.AdaptiveSwitchFDLeaks when
//
//	post.ActiveConnections + (post.RequestCount - pre.RequestCount)
//	  < pre.ActiveConnections
//
// — i.e. fewer connections survived than the request-served budget
// can account for. Probatorium's switch-FD-leak property reads the
// counter via the unix socket after each adaptive churn cell.
func validateSwitch(pre, post engine.EngineMetrics) {
	served := uint64(0)
	if post.RequestCount > pre.RequestCount {
		served = post.RequestCount - pre.RequestCount
	}
	if post.ActiveConnections+int64(served) < pre.ActiveConnections {
		validation.AdaptiveSwitchFDLeaks.Add(1)
	}
}
