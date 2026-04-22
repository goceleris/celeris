package scenarios

import (
	"github.com/goceleris/loadgen"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
)

// ChainProfile names the 4 middleware chains benchmarked per workload.
// See [ChainProfiles] for the canonical ordered list.
const (
	ChainNone      = "none"      // no middleware, baseline
	ChainMinimal   = "minimal"   // recovery + requestid + logger
	ChainAPI       = "api"       // minimal + cors + ratelimit + bodylimit
	ChainFullstack = "fullstack" // api + compress + etag + cache + session
)

// ChainProfiles is the canonical ordered list of middleware chains.
var ChainProfiles = []string{ChainNone, ChainMinimal, ChainAPI, ChainFullstack}

// ChainScenario benches a middleware chain on top of a representative
// workload (get-json, post-4k, churn). 4 chains × 3 workloads = 12 cells.
type ChainScenario struct {
	name     string
	chain    string
	workload string
}

// NewChainScenario constructs a [ChainScenario].
func NewChainScenario(name, chain, workload string) *ChainScenario {
	return &ChainScenario{name: name, chain: chain, workload: workload}
}

// Name implements [Scenario].
func (s *ChainScenario) Name() string { return s.name }

// Category implements [Scenario].
func (s *ChainScenario) Category() string { return CategoryChain }

// Chain returns the middleware chain identifier (one of the Chain*
// constants).
func (s *ChainScenario) Chain() string { return s.chain }

// WorkloadID returns the representative workload this chain sits on
// ("get-json", "post-4k", "churn").
func (s *ChainScenario) WorkloadID() string { return s.workload }

// Workload is a scaffold — wave-2 attaches the appropriate loadgen
// config for s.workload and the chain name is consumed by the server to
// decide which middleware stack to mount.
func (s *ChainScenario) Workload(target string) loadgen.Config {
	_ = target
	return loadgen.Config{}
}

// Applicable requires the server to declare Middleware=true.
func (s *ChainScenario) Applicable(fs servers.FeatureSet) bool {
	return fs.Middleware && (fs.HTTP1 || fs.HTTP2C || fs.Auto)
}

// Compile-time assertion that ChainScenario satisfies Scenario.
var _ Scenario = (*ChainScenario)(nil)
