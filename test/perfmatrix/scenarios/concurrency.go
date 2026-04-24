package scenarios

import (
	"github.com/goceleris/loadgen"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
)

// ConcurrencyProfile enumerates the per-target concurrency profiles the
// matrix sweeps: 1 connection, 128 connections, 1024 connections, and an
// auto-mix blend (H1 + H2 + H2C-upgrade, 1:1:1) produced via loadgen's
// -mix mode.
const (
	ProfileSingle  = "single-conn"
	ProfileMid     = "128-conn"
	ProfileHigh    = "1024-conn"
	ProfileAutoMix = "auto-mix-h1:h2:upgrade=1:1:1"
)

// ConcurrencyScenario parameterises a static workload with one of the
// four concurrency profiles listed above. Wave-2F registers four canonical
// cells ("get-json-1c", "get-simple-128c", "get-simple-1024c",
// "auto-mix-111"); richer cross-products can be layered on top by future
// waves without reshaping the type.
type ConcurrencyScenario struct {
	name    string
	profile string

	// Method is the HTTP method ("GET" for every registered profile).
	Method string

	// Path is the request path ("/" or "/json").
	Path string

	// Connections is the TCP connection count passed to loadgen. For the
	// auto-mix profile, loadgen spreads these connections across the three
	// protocol buckets according to Mix.
	Connections int

	// Mix, when non-nil, enables loadgen's weighted-draw protocol mixer
	// and MUST NOT be combined with HTTP2 / H2CUpgrade per loadgen's
	// Config godoc. Only the auto-mix scenario sets it.
	Mix *loadgen.MixRatio
}

// NewConcurrencyScenario constructs a [ConcurrencyScenario]. Kept for
// test ergonomics and backward compatibility with the scaffold signature.
func NewConcurrencyScenario(name, profile string) *ConcurrencyScenario {
	return &ConcurrencyScenario{name: name, profile: profile}
}

// Name implements [Scenario].
func (s *ConcurrencyScenario) Name() string { return s.name }

// Category implements [Scenario].
func (s *ConcurrencyScenario) Category() string { return CategoryConcurrency }

// Profile returns the concurrency profile identifier (one of [ProfileSingle],
// [ProfileMid], [ProfileHigh], [ProfileAutoMix]).
func (s *ConcurrencyScenario) Profile() string { return s.profile }

// Workload returns the loadgen.Config for this scenario. The orchestrator
// overwrites Duration and Warmup after calling Workload so CLI flags
// (-duration / -warmup) win; leaving them at zero here is intentional.
func (s *ConcurrencyScenario) Workload(target string) loadgen.Config {
	conns := s.Connections
	if conns <= 0 {
		conns = 1
	}
	method := s.Method
	if method == "" {
		method = "GET"
	}
	cfg := loadgen.Config{
		URL:         target + s.Path,
		Method:      method,
		Connections: conns,
	}
	// Mix is mutually exclusive with HTTP2 / H2CUpgrade per loadgen's
	// Config godoc — we deliberately do not set those flags here.
	if s.Mix != nil {
		mix := *s.Mix
		cfg.Mix = &mix
	}
	return cfg
}

// Applicable gates the auto-mix profile to servers that expose every
// wire-format the mixer draws from (H1, H2C prior-knowledge, and H2C
// upgrade). Every other profile drives plain H1 on the wire and is
// inapplicable to H2C-prior-knowledge-only servers (h2c-noupg) — those
// would silently record 0 RPS.
func (s *ConcurrencyScenario) Applicable(fs servers.FeatureSet) bool {
	if s.profile == ProfileAutoMix {
		return fs.HTTP1 && fs.HTTP2C && fs.H2CUpgrade
	}
	return fs.HTTP1
}

// Compile-time assertion that ConcurrencyScenario satisfies Scenario.
var _ Scenario = (*ConcurrencyScenario)(nil)

// ConcurrencyProfiles is the canonical ordered list of profiles.
var ConcurrencyProfiles = []string{
	ProfileSingle,
	ProfileMid,
	ProfileHigh,
	ProfileAutoMix,
}

func init() {
	Register(&ConcurrencyScenario{
		name:        "get-json-1c",
		profile:     ProfileSingle,
		Method:      "GET",
		Path:        "/json",
		Connections: 1,
	})
	Register(&ConcurrencyScenario{
		name:        "get-simple-128c",
		profile:     ProfileMid,
		Method:      "GET",
		Path:        "/",
		Connections: 128,
	})
	Register(&ConcurrencyScenario{
		name:        "get-simple-1024c",
		profile:     ProfileHigh,
		Method:      "GET",
		Path:        "/",
		Connections: 1024,
	})
	Register(&ConcurrencyScenario{
		name:        "auto-mix-111",
		profile:     ProfileAutoMix,
		Method:      "GET",
		Path:        "/",
		Connections: 64,
		Mix:         &loadgen.MixRatio{H1: 1, H2: 1, Upgrade: 1},
	})
}
