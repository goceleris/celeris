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

	// HTTP2, when true, drives loadgen in HTTP/2-cleartext prior-knowledge
	// mode — each worker opens an H2 connection and sends the client preface
	// directly (no HTTP/1.1 Upgrade handshake). Used to exercise H2C-capable
	// server cells, including h2c-noupg which refuses plain H1 by design.
	HTTP2 bool
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
	cfg := loadgen.Config{
		URL:              target + s.Path,
		Method:           method,
		Body:             s.Body,
		Connections:      conns,
		DisableKeepAlive: s.DisableKeepAlive,
		// Duration + Warmup intentionally left zero — filled by the
		// orchestrator from -duration / -warmup CLI flags (wave 2E).
	}
	if s.HTTP2 {
		cfg.HTTP2 = true
		// loadgen's HTTP/2 side has its own connection count knob —
		// reuse s.Connections so an H2 variant of an H1 scenario stays
		// comparable (same number of TCP connections, each carrying
		// multiplexed streams). Defaults for MaxStreams apply.
		cfg.HTTP2Options.Connections = conns
	}
	return cfg
}

// Applicable gates on the wire protocol the scenario actually drives.
// H1 variants (HTTP2=false) apply to any server that accepts plain
// HTTP/1.1; H2 variants (HTTP2=true) apply to any HTTP/2-cleartext-
// capable server, including h2c-noupg (refuses H1 but accepts the H2
// preface directly). Without this split, h2c-noupg cells silently
// record 0 RPS on H1 scenarios and H1-only servers would pair with H2
// scenarios they can't serve.
func (s *StaticScenario) Applicable(fs servers.FeatureSet) bool {
	if s.HTTP2 {
		return fs.HTTP2C
	}
	return fs.HTTP1
}

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
	// HTTP/2 prior-knowledge variants — exercise every HTTP2C-capable
	// server, including h2c-noupg (which refuses H1 entirely).
	"get-json-h2",
	"get-json-64k-h2",
	"post-4k-h2",
	"post-64k-h2",
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

	// HTTP/2 prior-knowledge variants. Paired with the H1 versions on the
	// same endpoints so an H2 regression shows up as a delta against its
	// H1 twin rather than an isolated number. Connection count is kept
	// lower than H1 because H2 multiplexes streams — 32 TCP conns ×
	// default 100 concurrent streams gives the same or higher effective
	// concurrency as H1's 128 conns.
	Register(&StaticScenario{
		name:        "get-json-h2",
		Method:      "GET",
		Path:        "/json",
		Connections: 32,
		HTTP2:       true,
	})
	Register(&StaticScenario{
		name:        "get-json-64k-h2",
		Method:      "GET",
		Path:        "/json-64k",
		Connections: 32,
		HTTP2:       true,
	})
	Register(&StaticScenario{
		name:        "post-4k-h2",
		Method:      "POST",
		Path:        "/upload",
		Body:        post4KBody,
		Connections: 32,
		HTTP2:       true,
	})
	Register(&StaticScenario{
		name:        "post-64k-h2",
		Method:      "POST",
		Path:        "/upload",
		Body:        post64KBody,
		Connections: 32,
		HTTP2:       true,
	})
}
