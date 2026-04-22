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

// ConcurrencyScenario parameterises a static scenario with one of the
// four concurrency profiles listed above. Wave-2 will register the full
// cross-product (8 static × 4 profiles = 32 cells).
type ConcurrencyScenario struct {
	name    string
	profile string
}

// NewConcurrencyScenario constructs a [ConcurrencyScenario].
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

// Workload is a scaffold — wave-2 fills in concurrency / mix config
// based on s.profile.
func (s *ConcurrencyScenario) Workload(target string) loadgen.Config {
	_ = target
	return loadgen.Config{}
}

// Applicable gates the auto-mix profile to servers that expose H2
// upgrade; everything else is HTTP1-compatible.
func (s *ConcurrencyScenario) Applicable(fs servers.FeatureSet) bool {
	if s.profile == ProfileAutoMix {
		return fs.Auto && fs.H2CUpgrade && fs.HTTP1 && fs.HTTP2C
	}
	return fs.HTTP1 || fs.HTTP2C || fs.Auto
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
