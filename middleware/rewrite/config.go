package rewrite

import (
	"fmt"

	"github.com/goceleris/celeris"
)

// Config defines the rewrite middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// Rules maps regex patterns to replacement strings. Each key is a
	// regular expression matched against the request path, and the value
	// is the replacement string. Capture groups ($1, $2, ...) in the
	// replacement are expanded using regexp.ReplaceAllString semantics.
	//
	// Keys are sorted alphabetically at init time to provide deterministic
	// first-match-wins ordering.
	Rules map[string]string

	// RedirectCode controls the rewrite behavior:
	//   - 0 (default): silent rewrite via SetPath (path is modified in-place)
	//   - 301, 302, 303, 307, 308: sends an HTTP redirect response
	RedirectCode int
}

func (cfg Config) validate() {
	if len(cfg.Rules) == 0 {
		panic("rewrite: Rules must not be empty")
	}
	if cfg.RedirectCode != 0 {
		switch cfg.RedirectCode {
		case 301, 302, 303, 307, 308:
			// valid redirect codes
		default:
			panic(fmt.Sprintf("rewrite: RedirectCode must be 0 (silent) or a redirect status (301, 302, 303, 307, 308), got %d", cfg.RedirectCode))
		}
	}
}
