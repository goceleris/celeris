package healthcheck

import (
	"fmt"
	"time"

	"github.com/goceleris/celeris"
)

// Exported path constants for the default probe endpoints.
const (
	DefaultLivePath  = "/livez"
	DefaultReadyPath = "/readyz"
	DefaultStartPath = "/startupz"
)

// Checker reports a probe's health status. The context is provided so
// checkers can inspect the request or respect deadlines.
type Checker func(c *celeris.Context) bool

// Config defines the healthcheck middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths is a list of request paths to exclude from healthcheck
	// interception. Matching is exact (no glob or prefix support).
	SkipPaths []string

	// LivePath is the URL path for the liveness probe.
	// Default: "/livez".
	LivePath string

	// ReadyPath is the URL path for the readiness probe.
	// Default: "/readyz".
	ReadyPath string

	// StartPath is the URL path for the startup probe.
	// Default: "/startupz".
	StartPath string

	// LiveChecker reports whether the process is alive.
	// Default: always true.
	LiveChecker Checker

	// ReadyChecker reports whether the service can accept traffic.
	// Default: always true.
	ReadyChecker Checker

	// StartChecker reports whether initial startup has completed.
	// Default: always true.
	StartChecker Checker

	// CheckerTimeout is the maximum duration to wait for a checker to
	// complete. If the checker does not return within this duration,
	// the probe returns 503 unavailable.
	//
	// Special values:
	//   0               — use default (5s)
	//   FastPathTimeout — run synchronously without goroutine/channel/context
	//                     overhead (recommended for trivial checkers that
	//                     cannot block; use the FastPathTimeout constant
	//                     rather than a raw negative literal)
	//
	// Default: 5s.
	CheckerTimeout time.Duration
}

// DefaultCheckerTimeout is the default timeout for health checkers.
const DefaultCheckerTimeout = 5 * time.Second

// FastPathTimeout disables the goroutine/channel/context timeout machinery
// and runs checkers synchronously inline. Use this for trivial checkers
// that cannot block.
const FastPathTimeout = -1 * time.Second

// defaultConfigCopy returns the default healthcheck middleware configuration.
// Each call returns a fresh copy.
func defaultConfigCopy() Config {
	return defaultConfig
}

var defaultConfig = Config{
	LivePath:       DefaultLivePath,
	ReadyPath:      DefaultReadyPath,
	StartPath:      DefaultStartPath,
	LiveChecker:    func(_ *celeris.Context) bool { return true },
	ReadyChecker:   func(_ *celeris.Context) bool { return true },
	StartChecker:   func(_ *celeris.Context) bool { return true },
	CheckerTimeout: DefaultCheckerTimeout,
}

func applyDefaults(cfg Config) Config {
	// Paths: empty string means "disabled" — do not fill with default.
	if cfg.LiveChecker == nil {
		cfg.LiveChecker = defaultConfig.LiveChecker
	}
	if cfg.ReadyChecker == nil {
		cfg.ReadyChecker = defaultConfig.ReadyChecker
	}
	if cfg.StartChecker == nil {
		cfg.StartChecker = defaultConfig.StartChecker
	}
	// 0 = use default timeout; -1 = fast-path (no goroutine/channel/context).
	if cfg.CheckerTimeout == 0 {
		cfg.CheckerTimeout = DefaultCheckerTimeout
	}
	return cfg
}

func (cfg Config) validate() {
	paths := [3]struct {
		name, value string
	}{
		{"LivePath", cfg.LivePath},
		{"ReadyPath", cfg.ReadyPath},
		{"StartPath", cfg.StartPath},
	}
	for _, p := range paths {
		if p.value == "" {
			continue // disabled probe
		}
		if p.value[0] != '/' {
			panic(fmt.Sprintf("healthcheck: %s %q must start with '/'", p.name, p.value))
		}
	}
	// Overlap checks only apply to enabled (non-empty) probes.
	if cfg.LivePath != "" && cfg.LivePath == cfg.ReadyPath {
		panic(fmt.Sprintf("healthcheck: LivePath and ReadyPath must differ, both are %q", cfg.LivePath))
	}
	if cfg.LivePath != "" && cfg.LivePath == cfg.StartPath {
		panic(fmt.Sprintf("healthcheck: LivePath and StartPath must differ, both are %q", cfg.LivePath))
	}
	if cfg.ReadyPath != "" && cfg.ReadyPath == cfg.StartPath {
		panic(fmt.Sprintf("healthcheck: ReadyPath and StartPath must differ, both are %q", cfg.ReadyPath))
	}
}
