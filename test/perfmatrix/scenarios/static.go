package scenarios

import (
	"github.com/goceleris/loadgen"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
)

// StaticScenario is the common shape of the 8 static cells: GET /,
// /json, /json-1k, /json-64k, POST 4k, POST 64k, POST 1m, churn.
//
// Wave-2 fills in Workload with loadgen.Config bodies. Until then
// Workload returns a zero-valued Config so the orchestrator can build
// without hitting nil panics.
type StaticScenario struct {
	name string
}

// NewStaticScenario constructs a [StaticScenario] with the given
// canonical name.
func NewStaticScenario(name string) *StaticScenario {
	return &StaticScenario{name: name}
}

// Name implements [Scenario].
func (s *StaticScenario) Name() string { return s.name }

// Category implements [Scenario].
func (s *StaticScenario) Category() string { return CategoryStatic }

// Workload is a scaffold — wave-2 fills it in with the real
// [loadgen.Config] (method, path, body size, mode, concurrency, etc.).
func (s *StaticScenario) Workload(target string) loadgen.Config {
	_ = target
	return loadgen.Config{}
}

// Applicable returns true for any server that speaks HTTP/1 or better.
// The tighter protocol gating is done by Scenario variants produced in
// wave-2 (e.g. post-1m-h2c requires HTTP2C=true).
func (s *StaticScenario) Applicable(fs servers.FeatureSet) bool {
	return fs.HTTP1 || fs.HTTP2C || fs.Auto
}

// Compile-time assertion that StaticScenario satisfies Scenario.
var _ Scenario = (*StaticScenario)(nil)

// StaticScenarioNames is the canonical ordered list of static cell-rows.
// Wave-2 wires real [Scenario] instances into Register for each.
var StaticScenarioNames = []string{
	"get-root",
	"get-json",
	"get-json-1k",
	"get-json-64k",
	"post-4k",
	"post-64k",
	"post-1m",
	"churn",
}
