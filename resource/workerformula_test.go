package resource

import (
	"testing"

	"github.com/goceleris/celeris/engine"
	"github.com/goceleris/celeris/internal/platform"
)

func TestResolveWorkers(t *testing.T) {
	smt2 := platform.CoreTopology{Logical: 16, Physical: 8, SMT: 2, NUMANodes: 1}
	noSMT := platform.CoreTopology{Logical: 8, Physical: 8, SMT: 1, NUMANodes: 1}
	tiny := platform.CoreTopology{Logical: 2, Physical: 1, SMT: 2, NUMANodes: 1}
	single := platform.CoreTopology{Logical: 1, Physical: 1, SMT: 1, NUMANodes: 1}

	tests := []struct {
		name string
		topo platform.CoreTopology
		eng  engine.EngineType
		hint WorkloadHint
		want int
	}{
		// io_uring sizes to Physical (one ring per physical core).
		{"iouring balanced SMT2", smt2, engine.IOUring, WorkloadBalanced, 8},
		{"iouring keepalive SMT2", smt2, engine.IOUring, WorkloadKeepAlive, 8},
		{"iouring highconc SMT2 (no special case)", smt2, engine.IOUring, WorkloadHighConcurrency, 8},
		{"iouring churn SMT2 halves rings", smt2, engine.IOUring, WorkloadChurn, 4},
		// epoll keeps Logical except under high concurrency.
		{"epoll balanced SMT2", smt2, engine.Epoll, WorkloadBalanced, 16},
		{"epoll keepalive SMT2", smt2, engine.Epoll, WorkloadKeepAlive, 16},
		{"epoll churn SMT2", smt2, engine.Epoll, WorkloadChurn, 16},
		{"epoll highconc SMT2 adds physical/2", smt2, engine.Epoll, WorkloadHighConcurrency, 20},
		// std / adaptive keep historical sizing.
		{"std balanced SMT2", smt2, engine.Std, WorkloadBalanced, 16},
		{"adaptive highconc SMT2", smt2, engine.Adaptive, WorkloadHighConcurrency, 16},

		// No-SMT host: Balanced is a no-op (every engine == Logical).
		{"iouring balanced noSMT == logical", noSMT, engine.IOUring, WorkloadBalanced, 8},
		{"epoll balanced noSMT == logical", noSMT, engine.Epoll, WorkloadBalanced, 8},
		{"std balanced noSMT == logical", noSMT, engine.Std, WorkloadBalanced, 8},
		{"epoll highconc noSMT adds physical/2", noSMT, engine.Epoll, WorkloadHighConcurrency, 12},

		// MinWorkers floor on tiny / single-core hosts.
		{"iouring balanced tiny floors to MinWorkers", tiny, engine.IOUring, WorkloadBalanced, MinWorkers},
		{"iouring churn tiny floors to MinWorkers", tiny, engine.IOUring, WorkloadChurn, MinWorkers},
		{"epoll highconc tiny floors to MinWorkers", tiny, engine.Epoll, WorkloadHighConcurrency, MinWorkers},
		{"single core floors to MinWorkers (clamped by 2*logical)", single, engine.Epoll, WorkloadBalanced, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveWorkers(tt.topo, tt.eng, tt.hint); got != tt.want {
				t.Errorf("ResolveWorkers(%+v, %v, %d) = %d, want %d",
					tt.topo, tt.eng, tt.hint, got, tt.want)
			}
		})
	}
}

// TestResolveWorkers_Clamps verifies the [MinWorkers, 2*Logical] bounds hold
// even for pathological topologies (e.g. a corrupt Physical > Logical reading).
func TestResolveWorkers_Clamps(t *testing.T) {
	// Physical reported larger than Logical (bad sysfs read) must not exceed
	// the upper clamp, and degenerate zero values must floor to MinWorkers.
	bad := platform.CoreTopology{Logical: 4, Physical: 99, SMT: 1, NUMANodes: 1}
	if got := ResolveWorkers(bad, engine.Epoll, WorkloadHighConcurrency); got < MinWorkers || got > 2*4 {
		t.Errorf("ResolveWorkers clamp violated: got %d, want within [%d, %d]", got, MinWorkers, 2*4)
	}
	zero := platform.CoreTopology{}
	if got := ResolveWorkers(zero, engine.IOUring, WorkloadBalanced); got < MinWorkers {
		t.Errorf("ResolveWorkers(zero topo) = %d, want >= MinWorkers(%d)", got, MinWorkers)
	}
}

// TestResolveWorkers_BalancedDefaultIsHistorical documents the safety property
// that the default hint never reduces epoll/std below the historical Logical
// sizing — only io_uring intentionally drops to Physical on SMT hosts.
func TestResolveWorkers_BalancedDefaultIsHistorical(t *testing.T) {
	for _, topo := range []platform.CoreTopology{
		{Logical: 8, Physical: 8, SMT: 1, NUMANodes: 1},
		{Logical: 16, Physical: 8, SMT: 2, NUMANodes: 1},
		{Logical: 32, Physical: 16, SMT: 2, NUMANodes: 2},
	} {
		for _, eng := range []engine.EngineType{engine.Epoll, engine.Std, engine.Adaptive} {
			if got := ResolveWorkers(topo, eng, WorkloadBalanced); got != topo.Logical {
				t.Errorf("ResolveWorkers(%+v, %v, Balanced) = %d, want Logical=%d",
					topo, eng, got, topo.Logical)
			}
		}
	}
}
