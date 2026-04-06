package rewrite

import (
	"regexp"
	"strings"

	"github.com/goceleris/celeris"
)

type compiledRule struct {
	pattern      *regexp.Regexp
	replacement  string
	redirectCode int             // 0 means use config-level default
	methods      map[string]bool // nil means all methods
	host         string          // empty means all hosts
}

// New creates a rewrite middleware with the given config. Rules are compiled
// into regular expressions at init time and evaluated in the order provided.
// The first matching regex wins and subsequent rules are not checked.
//
// Panics if Rules is empty or contains an invalid regex pattern
// (via regexp.MustCompile).
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	rules := make([]compiledRule, len(cfg.Rules))
	for i, r := range cfg.Rules {
		var methodMap map[string]bool
		if len(r.Methods) > 0 {
			methodMap = make(map[string]bool, len(r.Methods))
			for _, m := range r.Methods {
				methodMap[strings.ToUpper(m)] = true
			}
		}
		rules[i] = compiledRule{
			pattern:      regexp.MustCompile(r.Pattern),
			replacement:  r.Replacement,
			redirectCode: r.RedirectCode,
			methods:      methodMap,
			host:         r.Host,
		}
	}

	redirectCode := cfg.RedirectCode

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		path := c.Path()
		for _, r := range rules {
			if r.methods != nil && !r.methods[c.Method()] {
				continue
			}
			if r.host != "" && c.Host() != r.host {
				continue
			}
			if r.pattern.MatchString(path) {
				newPath := r.pattern.ReplaceAllString(path, r.replacement)

				// Per-rule RedirectCode overrides config-level default.
				code := r.redirectCode
				if code == 0 {
					code = redirectCode
				}

				if code != 0 {
					url := c.Scheme() + "://" + c.Host() + newPath
					if q := c.RawQuery(); q != "" {
						url += "?" + q
					}
					return c.Redirect(code, url)
				}

				c.SetPath(newPath)
				return c.Next()
			}
		}

		return c.Next()
	}
}
