package scenarios

import (
	"github.com/goceleris/loadgen"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
	"github.com/goceleris/celeris/test/perfmatrix/services"
)

// DriverKind names the 4 driver-backed scenarios.
const (
	DriverPG        = "driver-pg-read"
	DriverRedis     = "driver-redis-get"
	DriverMemcached = "driver-mc-get"
	DriverSession   = "driver-session-rw"
)

// DriverKinds is the canonical ordered list of driver scenarios.
var DriverKinds = []string{DriverPG, DriverRedis, DriverMemcached, DriverSession}

// sessionBody is the 256-byte payload POSTed by driver-session-rw. It is
// deterministic so repeat runs send byte-identical requests — any
// throughput delta then reflects server-side work rather than payload
// entropy.
var sessionBody = makeSessionBody()

func makeSessionBody() []byte {
	// 256 bytes of JSON-ish content: plain ASCII so every framework's
	// body parser treats it identically. Content exceeds the wire
	// minimum for a session write and matches the Seed blob size.
	const size = 256
	b := make([]byte, size)
	for i := range b {
		b[i] = 'x'
	}
	// Make it look like JSON so bodylimit / content-type sniffing
	// middlewares don't choke on it.
	b[0] = '{'
	b[1] = '"'
	b[2] = 'k'
	b[3] = '"'
	b[4] = ':'
	b[5] = '"'
	b[size-2] = '"'
	b[size-1] = '}'
	return b
}

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

// Workload returns the loadgen.Config for this driver scenario.
// driver-pg-read pins id=42 (seeded by services.Seed); driver-redis-get
// and driver-mc-get both request services.FixtureDemoKey; driver-session-rw
// POSTs a 256-byte payload to /session.
func (s *DriverScenario) Workload(target string) loadgen.Config {
	cfg := loadgen.Config{
		Connections: 128,
	}
	switch s.kind {
	case DriverPG:
		cfg.Method = "GET"
		// Fixed id=42 keeps the bench hitting the same row in PG's
		// buffer cache; the scenario is a bounded-reads-per-second
		// comparison, not a random-access workload.
		cfg.URL = target + "/db/user/42"
	case DriverRedis:
		cfg.Method = "GET"
		cfg.URL = target + "/cache/" + services.FixtureDemoKey
	case DriverMemcached:
		cfg.Method = "GET"
		cfg.URL = target + "/mc/" + services.FixtureDemoKey
	case DriverSession:
		cfg.Method = "POST"
		cfg.URL = target + "/session"
		cfg.Body = sessionBody
	}
	return cfg
}

// Applicable requires the server to declare Drivers=true and speak
// HTTP/1.1 on the wire. Driver scenarios drive the server with plain H1
// so a server that only accepts H2C prior-knowledge is skipped —
// otherwise every request is rejected at the parser and the cell
// silently records 0 RPS.
func (s *DriverScenario) Applicable(fs servers.FeatureSet) bool {
	return fs.Drivers && fs.HTTP1
}

// Compile-time assertion that DriverScenario satisfies Scenario.
var _ Scenario = (*DriverScenario)(nil)

func init() {
	Register(NewDriverScenario(DriverPG, DriverPG))
	Register(NewDriverScenario(DriverRedis, DriverRedis))
	Register(NewDriverScenario(DriverMemcached, DriverMemcached))
	Register(NewDriverScenario(DriverSession, DriverSession))
}
