package basicauth

import (
	"strings"

	"github.com/goceleris/celeris"
)

// ErrUnauthorized is returned when authentication fails.
var ErrUnauthorized = celeris.NewHTTPError(401, "Unauthorized")

// New creates a basic auth middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	skipMap := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skipMap[p] = struct{}{}
	}

	escapedRealm := strings.ReplaceAll(cfg.Realm, `\`, `\\`)
	escapedRealm = strings.ReplaceAll(escapedRealm, `"`, `\"`)
	realmHeader := `Basic realm="` + escapedRealm + `"`
	validator := cfg.Validator
	validatorCtx := cfg.ValidatorWithContext
	errorHandler := cfg.ErrorHandler

	if errorHandler == nil {
		errorHandler = func(c *celeris.Context, _ error) error {
			c.SetHeader("www-authenticate", realmHeader)
			c.SetHeader("cache-control", "no-store")
			c.SetHeader("vary", "authorization")
			return ErrUnauthorized
		}
	}

	return func(c *celeris.Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}

		if _, ok := skipMap[c.Path()]; ok {
			return c.Next()
		}

		user, pass, ok := c.BasicAuth()
		if !ok {
			return errorHandler(c, ErrUnauthorized)
		}

		var valid bool
		if validatorCtx != nil {
			valid = validatorCtx(c, user, pass)
		} else {
			valid = validator(user, pass)
		}
		if !valid {
			return errorHandler(c, ErrUnauthorized)
		}

		c.Set(UsernameKey, user)
		return c.Next()
	}
}

// UsernameFromContext returns the authenticated username from the context store.
// Returns an empty string if no username was stored (e.g., no auth or skipped).
func UsernameFromContext(c *celeris.Context) string {
	v, ok := c.Get(UsernameKey)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
