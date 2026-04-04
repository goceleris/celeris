package csrf

import (
	"strings"
	"time"

	"github.com/goceleris/celeris"
)

// Config defines the CSRF middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// TokenLength is the number of random bytes used to generate the token.
	// The resulting hex-encoded token string is twice this length.
	// Default: 32.
	TokenLength int

	// TokenLookup defines the source(s) for extracting the CSRF token on
	// unsafe methods. Format is "source:name" where source is "header",
	// "form", or "query". Multiple sources can be comma-separated; the
	// first non-empty match is used. Default: "header:X-CSRF-Token".
	TokenLookup string

	// CookieName is the name of the CSRF cookie. Default: "_csrf".
	CookieName string

	// CookiePath is the path attribute of the CSRF cookie. Default: "/".
	CookiePath string

	// CookieDomain is the domain attribute of the CSRF cookie.
	CookieDomain string

	// CookieMaxAge is the max-age of the CSRF cookie in seconds.
	// Default: 86400 (24 hours). Set to 0 for a session cookie
	// (deleted when the browser closes).
	CookieMaxAge int

	// CookieSecure flags the cookie for HTTPS-only transmission.
	CookieSecure bool

	// CookieHTTPOnly prevents client-side scripts from accessing the cookie.
	// Default: true. Set to false for SPA usage where JavaScript must
	// read the cookie.
	CookieHTTPOnly bool

	// CookieSameSite controls the SameSite attribute of the CSRF cookie.
	// Default: celeris.SameSiteLaxMode.
	CookieSameSite celeris.SameSite

	// ErrorHandler handles CSRF validation failures. When nil, the
	// middleware returns the error directly (ErrMissingToken or ErrForbidden).
	ErrorHandler func(c *celeris.Context, err error) error

	// SafeMethods lists HTTP methods that do not require token validation.
	// Default: ["GET", "HEAD", "OPTIONS", "TRACE"].
	SafeMethods []string

	// TrustedOrigins lists additional origins that are allowed for
	// cross-origin requests. Each entry should be a full origin
	// (e.g. "https://app.example.com") or a wildcard subdomain pattern
	// (e.g. "https://*.example.com"). When the Origin header is present
	// on unsafe methods, it must match the request Host or one of these
	// entries. An empty list means only same-origin requests are allowed.
	TrustedOrigins []string

	// Storage enables server-side token storage (signed double-submit).
	// When set, tokens are stored in the backend and validated against
	// the store on unsafe methods. When nil, pure double-submit cookie
	// mode is used.
	Storage Storage

	// Expiration is the token lifetime in server-side storage.
	// Only used when Storage is set. Default: 1 hour.
	Expiration time.Duration

	// SingleUseToken, when true and Storage is set, deletes the token
	// from storage after successful validation. A new token is generated
	// on the next safe-method request.
	SingleUseToken bool

	// KeyGenerator provides a custom token generation function.
	// When nil, the default buffered hex generator is used.
	KeyGenerator func() string

	// ContextKey is the key used to store the CSRF token in the
	// request context. Default: "csrf_token".
	ContextKey string
}

// defaultConfig is the default CSRF configuration.
var defaultConfig = Config{
	TokenLength:    32,
	TokenLookup:    "header:X-CSRF-Token",
	CookieName:     "_csrf",
	CookiePath:     "/",
	CookieMaxAge:   86400,
	CookieHTTPOnly: true,
	CookieSameSite: celeris.SameSiteLaxMode,
	SafeMethods:    []string{"GET", "HEAD", "OPTIONS", "TRACE"},
	Expiration:     time.Hour,
	ContextKey:     "csrf_token",
}

func applyDefaults(cfg Config) Config {
	if cfg.TokenLength <= 0 {
		cfg.TokenLength = defaultConfig.TokenLength
	}
	if cfg.TokenLookup == "" {
		cfg.TokenLookup = defaultConfig.TokenLookup
	}
	if cfg.CookieName == "" {
		cfg.CookieName = defaultConfig.CookieName
	}
	if cfg.CookiePath == "" {
		cfg.CookiePath = defaultConfig.CookiePath
	}
	if cfg.CookieSameSite == 0 {
		cfg.CookieSameSite = defaultConfig.CookieSameSite
	}
	if len(cfg.SafeMethods) == 0 {
		cfg.SafeMethods = defaultConfig.SafeMethods
	}
	if cfg.Expiration <= 0 {
		cfg.Expiration = defaultConfig.Expiration
	}
	if cfg.ContextKey == "" {
		cfg.ContextKey = defaultConfig.ContextKey
	}
	// CSRF cookies MUST be HTTPOnly — there is no legitimate use case for
	// JS-readable CSRF cookies in cookie-based double-submit. This is a
	// security invariant, not a preference.
	cfg.CookieHTTPOnly = true
	// CookieMaxAge: 0 means "session cookie" (no Max-Age attribute).
	// Only default when the value is negative (invalid); the defaultConfig
	// already carries 86400 for the no-args path.
	if cfg.CookieMaxAge < 0 {
		cfg.CookieMaxAge = defaultConfig.CookieMaxAge
	}
	return cfg
}

// wildcardOrigin represents a parsed wildcard trusted origin pattern.
// maxSubdomainDepth limits how many additional dot-separated labels the
// wildcard portion may contain, aligned with the CORS middleware behavior.
// A depth of 1 (the default) means "*.example.com" matches
// "sub.example.com" but NOT "a.b.example.com". Set to 0 for unlimited depth.
type wildcardOrigin struct {
	prefix            string
	suffix            string
	maxSubdomainDepth int
}

func (w wildcardOrigin) match(origin string) bool {
	if len(origin) < len(w.prefix)+len(w.suffix)+1 {
		return false
	}
	if !strings.HasPrefix(origin, w.prefix) || !strings.HasSuffix(origin, w.suffix) {
		return false
	}
	// Extract the middle portion that the wildcard matched.
	// Reject matches where it contains ".." or starts with "."
	// to prevent subdomain traversal attacks (e.g. ".evil.com"
	// matching "https://.evil.com.example.com").
	middle := origin[len(w.prefix) : len(origin)-len(w.suffix)]
	if strings.HasPrefix(middle, ".") || strings.Contains(middle, "..") {
		return false
	}
	if w.maxSubdomainDepth > 0 {
		dots := strings.Count(middle, ".")
		if dots >= w.maxSubdomainDepth {
			return false
		}
	}
	return true
}

func (cfg Config) validate() {
	validateExtractorNotCookie(cfg.TokenLookup, cfg.CookieName)
	validateTokenLookup(cfg.TokenLookup)
	if cfg.TokenLength > 32 {
		panic("csrf: TokenLength must not exceed 32")
	}
	if cfg.CookieSameSite == celeris.SameSiteNoneMode && !cfg.CookieSecure {
		panic("csrf: CookieSecure must be true when CookieSameSite is None")
	}
	if cfg.SingleUseToken && cfg.Storage == nil {
		panic("csrf: SingleUseToken requires Storage to be set")
	}
	for _, o := range cfg.TrustedOrigins {
		if strings.Contains(o, "*") && !strings.HasPrefix(o, "https://") {
			panic("csrf: wildcard TrustedOrigins must use https:// scheme, got " + o)
		}
	}
}

// validateExtractorNotCookie panics if any TokenLookup segment extracts
// from a cookie whose name matches CookieName. Extracting the CSRF token
// from the CSRF cookie itself defeats the double-submit pattern entirely:
// both the "cookie" and "request" tokens would always be identical,
// providing zero CSRF protection.
func validateExtractorNotCookie(lookup, cookieName string) {
	for _, seg := range strings.Split(lookup, ",") {
		seg = strings.TrimSpace(seg)
		parts := strings.SplitN(seg, ":", 2)
		if len(parts) != 2 {
			continue
		}
		if strings.ToLower(parts[0]) == "cookie" && parts[1] == cookieName {
			panic("csrf: TokenLookup extracts from cookie:" + cookieName +
				" which is the CSRF cookie itself — this defeats the double-submit pattern")
		}
	}
}

func validateTokenLookup(lookup string) {
	segments := strings.Split(lookup, ",")
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		parts := strings.SplitN(seg, ":", 2)
		if len(parts) != 2 || parts[1] == "" {
			panic("csrf: TokenLookup must be in the format \"source:name\" (e.g. \"header:X-CSRF-Token\")")
		}
		source := strings.ToLower(parts[0])
		if source != "header" && source != "form" && source != "query" {
			panic("csrf: TokenLookup source must be \"header\", \"form\", or \"query\", got \"" + parts[0] + "\"")
		}
	}
}
