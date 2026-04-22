package scenarios

import (
	"github.com/goceleris/loadgen"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
)

// ChainProfile names the 4 middleware chains benchmarked per workload.
// See [ChainProfiles] for the canonical ordered list.
const (
	ChainAPI       = "api"       // requestid + logger + recovery + cors
	ChainAuth      = "auth"      // api + basicauth
	ChainSecurity  = "security"  // auth + csrf + secure
	ChainFullstack = "fullstack" // security + ratelimit + timeout + bodylimit
)

// ChainProfiles is the canonical ordered list of middleware chains.
var ChainProfiles = []string{ChainAPI, ChainAuth, ChainSecurity, ChainFullstack}

// ChainWorkload identifies one of the three representative workloads each
// chain is exercised against: a small-JSON read, a 4 KiB upload, and a
// single-conn latency probe built on top of the same small-JSON handler.
const (
	ChainWorkloadGetJSON   = "get-json"
	ChainWorkloadPost4K    = "post-4k"
	ChainWorkloadGetJSON1C = "get-json-1c"
)

// chainPrefix maps a chain name to its HTTP path prefix mounted by each
// framework's chain_handlers.go. Kept here so both scenarios and server
// packages reference the same source of truth.
var chainPrefix = map[string]string{
	ChainAPI:       "/chain/api/",
	ChainAuth:      "/chain/auth/",
	ChainSecurity:  "/chain/security/",
	ChainFullstack: "/chain/fullstack/",
}

// ChainPrefix returns the HTTP path prefix used by chain `name`. It is
// also used by the framework-side chain_handlers.go files to mount the
// matching middleware stack under the same URL namespace.
func ChainPrefix(name string) string {
	p, ok := chainPrefix[name]
	if !ok {
		return ""
	}
	return p
}

// chainPost4KBody is a 4 KiB deterministic payload for POST /chain/*/upload
// across every chain. Seeded distinctly from the static post-4k body so a
// regression on one payload leaves the other byte-identical.
var chainPost4KBody = makeRandomBody(4*1024, 0xCA11_4411)

// basicAuthHeader carries base64("bench:bench") — the shared credential
// every /chain/auth/* and richer stack expects. Header value is RFC 7617.
const basicAuthHeader = "Basic YmVuY2g6YmVuY2g="

// ChainScenario benches a middleware chain on top of a representative
// workload (get-json, post-4k, get-json at single connection).
// 4 chains × 3 workloads = 12 cells.
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

// WorkloadID returns the representative workload this chain sits on.
func (s *ChainScenario) WorkloadID() string { return s.workload }

// Workload returns the loadgen.Config for this chain scenario.
// The 1-conn variant pins Connections=1; others pin 128.
// Chains that include basicauth (auth, security, fullstack) attach the
// shared bench:bench Authorization header so every request authenticates.
func (s *ChainScenario) Workload(target string) loadgen.Config {
	prefix := chainPrefix[s.chain]
	cfg := loadgen.Config{
		Connections: 128,
	}
	switch s.workload {
	case ChainWorkloadGetJSON:
		cfg.Method = "GET"
		cfg.URL = target + prefix + "json"
	case ChainWorkloadPost4K:
		cfg.Method = "POST"
		cfg.URL = target + prefix + "upload"
		cfg.Body = chainPost4KBody
	case ChainWorkloadGetJSON1C:
		cfg.Method = "GET"
		cfg.URL = target + prefix + "json"
		cfg.Connections = 1
	}
	// Any chain that mounts basicauth must authenticate — otherwise every
	// request is 401 and the cell's error count explodes.
	if s.chain == ChainAuth || s.chain == ChainSecurity || s.chain == ChainFullstack {
		if cfg.Headers == nil {
			cfg.Headers = make(map[string]string, 1)
		}
		cfg.Headers["Authorization"] = basicAuthHeader
	}
	return cfg
}

// Applicable requires the server to declare Middleware=true.
func (s *ChainScenario) Applicable(fs servers.FeatureSet) bool {
	return fs.Middleware && (fs.HTTP1 || fs.HTTP2C || fs.Auto)
}

// Compile-time assertion that ChainScenario satisfies Scenario.
var _ Scenario = (*ChainScenario)(nil)

// chainScenarioName returns the canonical name for a (chain, workload)
// pair, e.g. ("api", "get-json-1c") → "chain-api-get-json-1c".
func chainScenarioName(chain, workload string) string {
	return "chain-" + chain + "-" + workload
}

func init() {
	for _, chain := range ChainProfiles {
		for _, wl := range []string{ChainWorkloadGetJSON, ChainWorkloadPost4K, ChainWorkloadGetJSON1C} {
			Register(NewChainScenario(chainScenarioName(chain, wl), chain, wl))
		}
	}
}
