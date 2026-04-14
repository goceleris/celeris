package keyauth

import (
	"errors"
	"strings"

	"github.com/goceleris/celeris"
)

// ErrUnauthorized is returned when the API key is invalid.
// Do not mutate: this is a shared sentinel value used with errors.Is.
// ErrUnauthorized aliases [celeris.ErrUnauthorized] so cross-package
// errors.Is checks work.
var ErrUnauthorized = celeris.ErrUnauthorized

// ErrMissingKey is returned when no API key is found in the request.
// Do not mutate: this is a shared sentinel value used with errors.Is.
var ErrMissingKey = &celeris.HTTPError{Code: 401, Message: "Unauthorized", Err: errors.New("keyauth: missing API key")}

// New creates a key auth middleware with the given config.
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

	extract := parseKeyLookup(cfg.KeyLookup)
	validator := cfg.Validator
	successHandler := cfg.SuccessHandler
	errorHandler := cfg.ErrorHandler
	continueOnIgnored := cfg.ContinueOnIgnoredError
	wwwAuth := wwwAuthenticateValue(cfg)
	varyValue := headerVaryValue(cfg.KeyLookup)

	if errorHandler == nil {
		errorHandler = func(_ *celeris.Context, err error) error {
			return err
		}
	}

	handleError := func(c *celeris.Context, err error) error {
		herr := errorHandler(c, err)
		if herr == nil && continueOnIgnored {
			return c.Next()
		}
		if herr != nil {
			c.SetHeader("www-authenticate", wwwAuth)
			c.SetHeader("cache-control", "no-store")
			if varyValue != "" {
				c.AddHeader("vary", varyValue)
			}
		}
		return herr
	}

	return func(c *celeris.Context) error {
		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}

		if _, ok := skipMap[c.Path()]; ok {
			return c.Next()
		}

		// Defensive OPTIONS skip: CORS preflight must not be auth-blocked
		// regardless of middleware-installation order.
		if c.Method() == "OPTIONS" {
			return c.Next()
		}

		key := extract(c)
		if key == "" {
			return handleError(c, ErrMissingKey)
		}

		valid, err := validator(c, key)
		if err != nil {
			return handleError(c, err)
		}
		if !valid {
			return handleError(c, ErrUnauthorized)
		}

		c.Set(ContextKey, key)
		if successHandler != nil {
			successHandler(c)
		}
		return c.Next()
	}
}

// headerVaryValue extracts header names from a KeyLookup string and returns
// a comma-separated value suitable for a Vary response header. Only "header:..."
// sources contribute; query, cookie, form, and param sources are ignored.
// Returns "" if no header sources are present.
func headerVaryValue(lookup string) string {
	var names []string
	for _, part := range strings.Split(lookup, ",") {
		fields := strings.SplitN(strings.TrimLeft(part, " \t"), ":", 3)
		if len(fields) >= 2 && strings.TrimSpace(fields[0]) == "header" {
			names = append(names, strings.TrimSpace(fields[1]))
		}
	}
	return strings.Join(names, ", ")
}

// KeyFromContext returns the authenticated API key from the context store.
// Returns an empty string if no key was stored (e.g., no auth or skipped).
func KeyFromContext(c *celeris.Context) string {
	v, ok := c.Get(ContextKey)
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}
