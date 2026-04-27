package basicauth

import (
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"unsafe"

	"github.com/goceleris/celeris"
)

// ErrUnauthorized is returned when authentication fails.
// ErrUnauthorized aliases [celeris.ErrUnauthorized] so cross-package
// errors.Is checks work.
var ErrUnauthorized = celeris.ErrUnauthorized

// New creates a basic auth middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	// Capture pre-defaults flags to detect "user supplied only Users (no
	// custom validator)" — the only shape eligible for the static-user
	// fast path.
	rawValidatorNil := cfg.Validator == nil && cfg.ValidatorWithContext == nil
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
			// AddHeader (not SetHeader) to preserve Vary values set by
			// other middleware — see middleware/doc.go "Vary Header Convention".
			c.AddHeader("vary", "authorization")
			return ErrUnauthorized
		}
	}

	// Single-user static fast path: when only Users (one entry) is configured
	// and no custom validator is supplied, we can pre-encode the expected
	// "Basic <b64(user:pass)>" Authorization header at init and compare the
	// incoming header against it via [subtle.ConstantTimeCompare] — no
	// per-request base64 decode, no HMAC, no allocation. Mirrors the same
	// constant-time pattern used by every other framework's basicauth stub.
	//
	// Fall-through to the general path covers: HashedUsers, multi-user maps
	// (where the slow path's HMAC-with-dummy-compare keeps lookup timing
	// equal across hit/miss), and any user-supplied Validator.
	if rawValidatorNil && len(cfg.HashedUsers) == 0 && len(cfg.Users) == 1 {
		var (
			expectedHeader []byte
			expectedUser   string
		)
		for u, p := range cfg.Users {
			expectedHeader = []byte("Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+p)))
			expectedUser = u
		}
		successFn := cfg.SuccessHandler
		return func(c *celeris.Context) error {
			if cfg.Skip != nil && cfg.Skip(c) {
				return c.Next()
			}
			if _, ok := skipMap[c.Path()]; ok {
				return c.Next()
			}
			if c.Method() == "OPTIONS" {
				return c.Next()
			}
			auth := c.Header("authorization")
			if subtle.ConstantTimeCompare(
				unsafe.Slice(unsafe.StringData(auth), len(auth)),
				expectedHeader,
			) != 1 {
				return errorHandler(c, ErrUnauthorized)
			}
			c.SetString(UsernameKey, expectedUser)
			if successFn != nil {
				successFn(c)
			}
			return c.Next()
		}
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

		// SetString avoids the any-interface boxing c.Set(UsernameKey, user)
		// would pay — ~16B alloc per authenticated request.
		// UsernameFromContext reads via GetString, zero-alloc. User code
		// calling c.Get(UsernameKey) still sees the value (via fallback;
		// re-boxes on each call).
		c.SetString(UsernameKey, user)
		if cfg.SuccessHandler != nil {
			cfg.SuccessHandler(c)
		}
		return c.Next()
	}
}

// UsernameFromContext returns the authenticated username from the context store.
// Returns an empty string if no username was stored (e.g., no auth or skipped).
func UsernameFromContext(c *celeris.Context) string {
	s, _ := c.GetString(UsernameKey)
	return s
}
