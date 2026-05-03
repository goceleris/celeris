// Package servers defines the Server interface and Registry used by the
// perfmatrix orchestrator. Each benched framework lives in its own
// sub-package (servers/celeris, servers/fiber, ...) which registers its
// Server implementations during init().
package servers

import (
	"context"
	"net"
	"sort"
	"sync"

	"github.com/goceleris/celeris/test/perfmatrix/services"
)

// Server is one benched server instance: a specific framework + protocol +
// option set. Each Registry entry corresponds to one cell-column in the
// matrix.
type Server interface {
	// Name is the canonical cell-column identifier. Stable across runs.
	// Examples: "celeris-iouring-auto-upgrade+async", "fiber-h1",
	// "stdhttp-h2c".
	Name() string

	// Kind is the coarse framework bucket (celeris/fiber/fasthttp/...).
	Kind() string

	// Features reports which scenarios this server supports. Protocol-gated
	// cells use this to skip (e.g. fiber returns false for anything H2).
	Features() FeatureSet

	// Start launches the server on an ephemeral port. The returned
	// listener's address is the bench target. Handlers for every scenario
	// (/json, /db/user/:id, /chain/api/*, etc.) MUST be registered before
	// Start returns. Services is non-nil when driver scenarios are enabled.
	Start(ctx context.Context, svcs *services.Handles) (net.Listener, error)

	// Stop gracefully shuts the server down; bounded by ctx deadline.
	Stop(ctx context.Context) error
}

// FeatureSet advertises which scenario categories a given Server can host.
// Scenario.Applicable filters against this set so mismatched
// (server, scenario) pairs are skipped by the scheduler.
type FeatureSet struct {
	HTTP1         bool
	HTTP2C        bool
	Auto          bool
	H2CUpgrade    bool
	Drivers       bool // can it host driver scenarios
	Middleware    bool // can it host middleware-chain scenarios
	AsyncHandlers bool // relevant only for celeris configs
}

var (
	registryMu sync.RWMutex
	registry   = make(map[string]Server)
)

// Register adds s to the package-level registry. It is safe for concurrent
// use and is normally invoked from the init() of each servers/<framework>/
// sub-package. Duplicate names panic — every cell-column must be unique.
func Register(s Server) {
	if s == nil {
		panic("perfmatrix/servers: Register called with nil Server")
	}
	name := s.Name()
	if name == "" {
		panic("perfmatrix/servers: Register called with empty Name")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic("perfmatrix/servers: duplicate Server name " + name)
	}
	registry[name] = s
}

// Registry returns every registered Server sorted by Name. The slice is a
// freshly allocated copy, safe to mutate.
func Registry() []Server {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Server, 0, len(registry))
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
	registry = make(map[string]Server)
}
