package scenarios

import (
	"github.com/goceleris/loadgen"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
)

// DriverKind names the 4 driver-backed scenarios.
const (
	DriverPG        = "driver-pg-read"
	DriverRedis     = "driver-redis-get"
	DriverMemcached = "driver-memcached-get"
	DriverSession   = "driver-session-rw"
)

// DriverKinds is the canonical ordered list of driver scenarios.
var DriverKinds = []string{DriverPG, DriverRedis, DriverMemcached, DriverSession}

// DriverScenario benches a single hot-path call through a driver (PG
// read of user id=42, Redis GET demo, memcached GET demo, session read/
// write round-trip).
type DriverScenario struct {
	name string
	kind string
}

// NewDriverScenario constructs a [DriverScenario].
func NewDriverScenario(name, kind string) *DriverScenario {
	return &DriverScenario{name: name, kind: kind}
}

// Name implements [Scenario].
func (s *DriverScenario) Name() string { return s.name }

// Category implements [Scenario].
func (s *DriverScenario) Category() string { return CategoryDriver }

// Kind returns the driver kind (one of [DriverPG], [DriverRedis],
// [DriverMemcached], [DriverSession]).
func (s *DriverScenario) Kind() string { return s.kind }

// Workload is a scaffold — wave-2 wires the per-kind loadgen config
// (path under /driver/*, body if any, keep-alive, etc.).
func (s *DriverScenario) Workload(target string) loadgen.Config {
	_ = target
	return loadgen.Config{}
}

// Applicable requires the server to declare Drivers=true and to speak at
// least HTTP/1.
func (s *DriverScenario) Applicable(fs servers.FeatureSet) bool {
	return fs.Drivers && (fs.HTTP1 || fs.HTTP2C || fs.Auto)
}

// Compile-time assertion that DriverScenario satisfies Scenario.
var _ Scenario = (*DriverScenario)(nil)
