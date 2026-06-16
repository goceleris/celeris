package resource

import (
	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/platform"
)

// WorkloadHint coarsely classifies the connection workload so the worker
// formula can trade off per engine. WorkloadBalanced is the safe default and
// reproduces the historical numCPU sizing for every engine.
type WorkloadHint uint8

const (
	// WorkloadBalanced is the default: no workload-specific adjustment.
	WorkloadBalanced WorkloadHint = iota
	// WorkloadKeepAlive favors long-lived connections (few, busy workers).
	WorkloadKeepAlive
	// WorkloadChurn favors connection churn (new conn per request).
	WorkloadChurn
	// WorkloadHighConcurrency favors very high concurrent connection counts.
	WorkloadHighConcurrency
)

// ResolveWorkers computes the number of I/O worker loops for an engine from CPU
// topology and a workload hint. It encodes the v1.5.1 per-engine hypotheses
// (celeris#340); callers still apply engine-specific clamps afterwards (e.g.
// io_uring's RLIMIT_MEMLOCK cap). The result is clamped to [MinWorkers,
// 2*Logical].
//
// On a host WITHOUT SMT (Physical == Logical) the formula is a no-op for the
// default WorkloadBalanced hint: every engine resolves to Logical, exactly the
// historical GOMAXPROCS sizing. SMT and the non-default hints are where it
// diverges — io_uring sizes to Physical (one ring per physical core), and epoll
// under WorkloadHighConcurrency adds Physical/2 extra accept loops. This
// function is intentionally not yet wired into engine startup; per-engine
// adoption is gated on cluster A/B validation (celeris#340).
func ResolveWorkers(topo platform.CoreTopology, eng engine.EngineType, hint WorkloadHint) int {
	logical := topo.Logical
	if logical < 1 {
		logical = 1
	}
	physical := topo.Physical
	if physical < 1 || physical > logical {
		physical = logical
	}

	var n int
	switch eng {
	case engine.IOUring:
		switch hint {
		case WorkloadChurn:
			// Churn is pathological for io_uring (celeris#331); keep the ring
			// fleet small so there are fewer accept/close arenas to service.
			n = physical / 2
		default:
			// One ring per physical core: SMT siblings contend on the shared
			// per-core ring submission path, and each ring also costs ~12 MiB
			// of locked memory, so logical-core sizing over-provisions rings.
			n = physical
		}
	case engine.Epoll:
		switch hint {
		case WorkloadHighConcurrency:
			// More accept loops than cores improves SO_REUSEPORT fairness when
			// loops briefly block in syscalls at very high connection counts —
			// the get-simple-1024c gap vs gnet (celeris#338).
			n = logical + physical/2
		default:
			n = logical
		}
	default:
		// Std / Adaptive / default: leave the historical sizing.
		n = logical
	}

	if n < MinWorkers {
		n = MinWorkers
	}
	if maxN := 2 * logical; n > maxN {
		n = maxN
	}
	return n
}
