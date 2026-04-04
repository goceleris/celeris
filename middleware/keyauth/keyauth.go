package keyauth

import (
	"errors"

	"github.com/goceleris/celeris"
)

// ErrUnauthorized is returned when the API key is invalid.
// Do not mutate: this is a shared sentinel value used with errors.Is.
var ErrUnauthorized = celeris.NewHTTPError(401, "Unauthorized")

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
