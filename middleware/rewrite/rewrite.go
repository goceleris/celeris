package rewrite

import (
	"regexp"

	"github.com/goceleris/celeris"
)

type compiledRule struct {
	pattern     *regexp.Regexp
	replacement string
}

// New creates a rewrite middleware with the given config. Rules are compiled
// into regular expressions at init time and evaluated in the order provided.
// The first matching regex wins and subsequent rules are not checked.
//
// Panics if Rules is empty or contains an invalid regex pattern
// (via regexp.MustCompile).
func New(config Config) celeris.HandlerFunc {
	config.validate()

	rules := make([]compiledRule, len(config.Rules))
	for i, r := range config.Rules {
		rules[i] = compiledRule{
			pattern:     regexp.MustCompile(r.Pattern),
			replacement: r.Replacement,
		}
	}

	redirectCode := config.RedirectCode

	var skip celeris.SkipHelper
	skip.Init(config.SkipPaths, config.Skip)

	return func(c *celeris.Context) error {
		if skip.ShouldSkip(c) {
			return c.Next()
		}

		path := c.Path()
		for _, r := range rules {
			if r.pattern.MatchString(path) {
				newPath := r.pattern.ReplaceAllString(path, r.replacement)

				if redirectCode != 0 {
					url := c.Scheme() + "://" + c.Host() + newPath
					if q := c.RawQuery(); q != "" {
						url += "?" + q
					}
					return c.Redirect(redirectCode, url)
				}

				c.SetPath(newPath)
				return c.Next()
			}
		}

		return c.Next()
	}
}
