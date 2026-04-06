package rewrite

import (
	"fmt"

	"github.com/goceleris/celeris"
)

// Rule defines a single rewrite rule.
type Rule struct {
	// Pattern is a Go regular expression matched against the request path.
	// Use ^ and $ anchors for exact path matching.
	Pattern string
	// Replacement is the replacement string with capture group support ($1, $2).
	Replacement string
	// RedirectCode overrides [Config].RedirectCode for this rule.
	// When non-zero, this rule sends an HTTP redirect instead of a silent rewrite.
	// When zero (default), the config-level RedirectCode is used.
	RedirectCode int
}

// Config defines the rewrite middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// Rules defines the rewrite rules. First match wins.
	// Rules are evaluated in the order provided (not sorted).
	Rules []Rule

	// RedirectCode controls the rewrite behavior:
	//   - 0 (default): silent rewrite via SetPath (path is modified in-place)
	//   - 301, 302, 303, 307, 308: sends an HTTP redirect response
	RedirectCode int
}

// defaultConfig holds zero-value defaults. Rewrite has no meaningful defaults
// (Rules is required), but the variable is present for pattern consistency.
var defaultConfig Config

func applyDefaults(cfg Config) Config {
	return cfg
}

func (cfg Config) validate() {
	if len(cfg.Rules) == 0 {
		panic("rewrite: Rules must not be empty")
	}
	if cfg.RedirectCode != 0 {
		validateRedirectCode("RedirectCode", cfg.RedirectCode)
	}
	for i, r := range cfg.Rules {
		if r.Pattern == "" {
			panic(fmt.Sprintf("rewrite: Rules[%d].Pattern must not be empty", i))
		}
		if r.RedirectCode != 0 {
			validateRedirectCode(fmt.Sprintf("Rules[%d].RedirectCode", i), r.RedirectCode)
		}
	}
}

func validateRedirectCode(field string, code int) {
	switch code {
	case 301, 302, 303, 307, 308:
		// valid
	default:
		panic(fmt.Sprintf("rewrite: %s must be 0 (silent) or a redirect status (301, 302, 303, 307, 308), got %d", field, code))
	}
}
