// Package scenarios enumerates every benchable workload (cell-row in the
// matrix). Each Scenario knows how to configure loadgen and reports which
// FeatureSet it requires so the scheduler can skip incompatible (server,
// scenario) pairs.
package scenarios

import (
	"sort"
	"sync"

	"github.com/goceleris/loadgen"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
)

// Category groups scenarios for report sections.
const (
	CategoryStatic      = "static"
	CategoryConcurrency = "concurrency"
	CategoryChain       = "chain"
	CategoryDriver      = "driver"
)

// Scenario is one benchable workload — it knows how to configure loadgen
// and how to interpret the result.
type Scenario interface {
	// Name is the canonical cell-row identifier. Examples: "get-json",
	// "post-1m", "chain-fullstack-get-json", "driver-pg-read",
	// "auto-mix-h1:h2:upgrade=1:1:1".
	Name() string

	// Category groups scenarios for report sections: "static",
	// "concurrency", "chain", "driver".
	Category() string

	// Workload returns the loadgen.Config for this scenario. Duration and
	// Warmup are filled in by the orchestrator from -duration / -warmup
	// flags.
	Workload(target string) loadgen.Config

	// Applicable returns true if this scenario is runnable against the
	// given FeatureSet. The orchestrator skips mismatched
	// (server, scenario) pairs.
	Applicable(servers.FeatureSet) bool
}

var (
	registryMu sync.RWMutex
	registry   = make(map[string]Scenario)
)

// Register adds s to the scenarios registry. Duplicate names panic.
// Safe for concurrent use; normally called from package init().
func Register(s Scenario) {
	if s == nil {
		panic("perfmatrix/scenarios: Register called with nil Scenario")
	}
	name := s.Name()
	if name == "" {
		panic("perfmatrix/scenarios: Register called with empty Name")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic("perfmatrix/scenarios: duplicate Scenario name " + name)
	}
	registry[name] = s
}

// Registry returns every registered Scenario sorted by Name. The slice is
// a freshly allocated copy, safe to mutate.
func Registry() []Scenario {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Scenario, 0, len(registry))
	for _, s := range registry {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Reset clears the registry. Intended for tests only.
func Reset() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = make(map[string]Scenario)
}
