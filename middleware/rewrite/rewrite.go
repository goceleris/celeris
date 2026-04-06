package rewrite

import (
	"regexp"
	"slices"

	"github.com/goceleris/celeris"
)

type rule struct {
	pattern     *regexp.Regexp
	replacement string
}

// New creates a rewrite middleware with the given config. Rules are compiled
// into regular expressions at init time and sorted alphabetically by pattern
// string for deterministic first-match-wins ordering.
//
// Panics if Rules is empty or contains an invalid regex pattern
// (via regexp.MustCompile).
func New(config Config) celeris.HandlerFunc {
	config.validate()

	keys := make([]string, 0, len(config.Rules))
	for k := range config.Rules {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	rules := make([]rule, len(keys))
	for i, k := range keys {
		rules[i] = rule{
			pattern:     regexp.MustCompile(k),
			replacement: config.Rules[k],
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
