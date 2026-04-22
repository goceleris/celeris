package scenarios

import (
	"math/rand/v2"

	"github.com/goceleris/loadgen"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
)

// StaticScenario is the common shape of the 8 static cells: GET /,
// /json, /json-1k, /json-64k, POST 4k, POST 64k, POST 1m, and the churn
// variant that forces Connection: close.
//
// The zero value is not useful; construct instances via the init() block
// below (or via [NewStaticScenario] in tests) so the fields that drive
// loadgen are set explicitly and the payload byte slices are deterministic.
type StaticScenario struct {
	name string

	// Method is the HTTP method ("GET" or "POST").
	Method string

	// Path is the request path ("/", "/json", "/upload", ...).
	Path string

	// Body is the pre-generated request payload. nil for GET.
	Body []byte

	// Connections is the TCP connection count loadgen should dial.
	Connections int

	// DisableKeepAlive, when true, drives loadgen into Connection: close
	// mode (one request per conn). Used by the churn scenario.
	DisableKeepAlive bool
}

// NewStaticScenario constructs a [StaticScenario] with the given
// canonical name. Kept for test ergonomics and backward compatibility
// with the scaffold signature; callers that want real workload fields
// should use a struct literal.
func NewStaticScenario(name string) *StaticScenario {
	return &StaticScenario{name: name}
}

// Name implements [Scenario].
func (s *StaticScenario) Name() string { return s.name }

// Category implements [Scenario].
func (s *StaticScenario) Category() string { return CategoryStatic }

// Workload returns the loadgen.Config for this scenario. The
// orchestrator overwrites Duration and Warmup after calling Workload so
// CLI flags (-duration / -warmup) win; leaving them at zero here is
// intentional.
func (s *StaticScenario) Workload(target string) loadgen.Config {
	conns := s.Connections
	if conns <= 0 {
		conns = 128
	}
	method := s.Method
	if method == "" {
		method = "GET"
	}
	return loadgen.Config{
		URL:              target + s.Path,
		Method:           method,
		Body:             s.Body,
		Connections:      conns,
		DisableKeepAlive: s.DisableKeepAlive,
		// Duration + Warmup intentionally left zero — filled by the
		// orchestrator from -duration / -warmup CLI flags (wave 2E).
	}
}

// Applicable returns true for every server. Static scenarios are
// protocol-agnostic: any server that speaks HTTP at all can serve GET /
// and POST /upload regardless of H1/H2/auto-upgrade posture. Protocol
// gating lives on ConcurrencyScenario.auto-mix-111 and on chain/driver
// scenarios, not here.
func (s *StaticScenario) Applicable(servers.FeatureSet) bool { return true }

// Compile-time assertion that StaticScenario satisfies Scenario.
var _ Scenario = (*StaticScenario)(nil)

// StaticScenarioNames is the canonical ordered list of static cell-rows.
// Consumers that enumerate scenarios by name (report tables, CLI filters)
// use this slice rather than iterating the registry so ordering stays
// stable across runs.
var StaticScenarioNames = []string{
	"get-simple",
	"get-json",
	"get-json-1k",
	"get-json-64k",
	"post-4k",
	"post-64k",
	"post-1m",
	"churn-close",
}

// Pre-generated POST payloads. They are built once at package init() so
// every run (and every registered scenario) observes byte-identical
// bodies — any regression on server-side body parsing then shows up as a
// throughput delta rather than a content-size artefact.
//
// Each payload uses its own deterministic seed so a future change to one
// size leaves the others byte-identical.
var (
	post4KBody  = makeRandomBody(4*1024, 0xA11CE_4000)
	post64KBody = makeRandomBody(64*1024, 0xB0B_64000)
	post1MBody  = makeRandomBody(1024*1024, 0xC0DE_10000)
)

// makeRandomBody returns a byte slice of exactly n bytes filled with a
// deterministic PRNG keyed by seed. math/rand/v2's ChaCha8 is allocation-
// free after construction and produces enough entropy that the result
// does not compress — important when benching content-length parsers and
// keep-alive framing under load.
func makeRandomBody(n int, seed uint64) []byte {
	r := rand.New(rand.NewChaCha8([32]byte{
		byte(seed), byte(seed >> 8), byte(seed >> 16), byte(seed >> 24),
		byte(seed >> 32), byte(seed >> 40), byte(seed >> 48), byte(seed >> 56),
		0x9E, 0x37, 0x79, 0xB9, 0x7F, 0x4A, 0x7C, 0x15,
		0xF3, 0x9C, 0xC0, 0x60, 0x5C, 0xED, 0xC8, 0x34,
		0x10, 0x82, 0x27, 0x6B, 0xF3, 0xA2, 0x72, 0x51,
	}))
	b := make([]byte, n)
	// Fill in 8-byte chunks using Uint64 to keep the loop tight;
	// tail bytes handled explicitly to guarantee an exactly-n slice.
	i := 0
	for ; i+8 <= n; i += 8 {
		u := r.Uint64()
		b[i+0] = byte(u)
		b[i+1] = byte(u >> 8)
		b[i+2] = byte(u >> 16)
		b[i+3] = byte(u >> 24)
		b[i+4] = byte(u >> 32)
		b[i+5] = byte(u >> 40)
		b[i+6] = byte(u >> 48)
		b[i+7] = byte(u >> 56)
	}
	if i < n {
		u := r.Uint64()
		for j := 0; i < n; i, j = i+1, j+1 {
			b[i] = byte(u >> (8 * j))
		}
	}
	return b
}

func init() {
	Register(&StaticScenario{
		name:        "get-simple",
		Method:      "GET",
		Path:        "/",
		Connections: 128,
	})
	Register(&StaticScenario{
		name:        "get-json",
		Method:      "GET",
		Path:        "/json",
		Connections: 128,
	})
	Register(&StaticScenario{
		name:        "get-json-1k",
		Method:      "GET",
		Path:        "/json-1k",
		Connections: 128,
	})
	Register(&StaticScenario{
		name:        "get-json-64k",
		Method:      "GET",
		Path:        "/json-64k",
		Connections: 128,
	})
	Register(&StaticScenario{
		name:        "post-4k",
		Method:      "POST",
		Path:        "/upload",
		Body:        post4KBody,
		Connections: 128,
	})
	Register(&StaticScenario{
		name:        "post-64k",
		Method:      "POST",
		Path:        "/upload",
		Body:        post64KBody,
		Connections: 128,
	})
	Register(&StaticScenario{
		name:        "post-1m",
		Method:      "POST",
		Path:        "/upload",
		Body:        post1MBody,
		Connections: 128,
	})
	Register(&StaticScenario{
		name:             "churn-close",
		Method:           "GET",
		Path:             "/",
		Connections:      32,
		DisableKeepAlive: true,
	})
}
