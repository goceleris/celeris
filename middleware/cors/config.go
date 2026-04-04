package cors

import (
	"strconv"
	"strings"

	"github.com/goceleris/celeris"
)

// maxOriginLength is the maximum allowed length for an Origin header value.
// Origins exceeding this are rejected before any comparison to prevent
// CPU waste on extremely long strings.
const maxOriginLength = 256

// Config defines the CORS middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match on c.Path()).
	// Requests matching these paths bypass CORS processing entirely.
	SkipPaths []string

	// AllowOrigins lists allowed origins. Default: ["*"].
	AllowOrigins []string

	// AllowOriginsFunc is a custom function to validate origins.
	// Called after static and wildcard origin checks.
	// Cannot be used with wildcard "*" AllowOrigins.
	//
	// The middleware validates that the incoming Origin header looks like a
	// serialized origin (has scheme + host, no path/query/fragment/userinfo)
	// before passing it to this function. Malformed origins are rejected.
	AllowOriginsFunc func(origin string) bool

	// AllowOriginRequestFunc validates origins with access to the full request context.
	// Called after AllowOriginsFunc. Useful for tenant-based or header-dependent
	// origin decisions. Cannot be used with wildcard "*" AllowOrigins.
	//
	// The same serialized-origin validation applies as for AllowOriginsFunc.
	AllowOriginRequestFunc func(c *celeris.Context, origin string) bool

	// AllowMethods lists allowed HTTP methods.
	// Default: [GET, POST, PUT, DELETE, PATCH, HEAD, OPTIONS].
	AllowMethods []string

	// AllowHeaders lists allowed request headers.
	// Default: [Origin, Content-Type, Accept, Authorization].
	AllowHeaders []string

	// MirrorRequestHeaders, when true, causes the middleware to reflect
	// the Access-Control-Request-Headers value back in preflight responses
	// instead of using a fixed AllowHeaders list. This is useful when the
	// full set of request headers is not known ahead of time. When false
	// (the default), the AllowHeaders list is used.
	MirrorRequestHeaders bool

	// ExposeHeaders lists headers the browser can access.
	ExposeHeaders []string

	// AllowCredentials indicates whether credentials are allowed.
	AllowCredentials bool

	// AllowPrivateNetwork enables the Private Network Access spec.
	// When true, preflight responses include Access-Control-Allow-Private-Network.
	AllowPrivateNetwork bool

	// MaxAge is the preflight cache duration in seconds. Default: 0 (no cache).
	MaxAge int
}

// defaultConfig is the default CORS configuration.
var defaultConfig = Config{
	AllowOrigins: []string{"*"},
	AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"},
	AllowHeaders: []string{"Origin", "Content-Type", "Accept", "Authorization"},
}

func applyDefaults(cfg Config) Config {
	if len(cfg.AllowOrigins) == 0 {
		cfg.AllowOrigins = defaultConfig.AllowOrigins
	}
	if len(cfg.AllowMethods) == 0 {
		cfg.AllowMethods = defaultConfig.AllowMethods
	}
	if len(cfg.AllowHeaders) == 0 && !cfg.MirrorRequestHeaders {
		cfg.AllowHeaders = defaultConfig.AllowHeaders
	}
	return cfg
}

func (cfg Config) validate() {
	if cfg.AllowCredentials {
		for _, o := range cfg.AllowOrigins {
			if o == "*" {
				panic("cors: AllowCredentials cannot be used with wildcard AllowOrigins")
			}
		}
	}
	if cfg.AllowOriginsFunc != nil {
		for _, o := range cfg.AllowOrigins {
			if o == "*" {
				panic("cors: AllowOriginsFunc cannot be used with wildcard AllowOrigins")
			}
		}
	}
	if cfg.AllowOriginRequestFunc != nil {
		for _, o := range cfg.AllowOrigins {
			if o == "*" {
				panic("cors: AllowOriginRequestFunc cannot be used with wildcard AllowOrigins")
			}
		}
	}
}

// wildcardPattern represents a parsed wildcard origin like "https://*.example.com".
// maxSubdomainDepth limits how many additional dot-separated labels the wildcard
// portion may contain. A depth of 1 (the default) means "*.example.com" matches
// "sub.example.com" but NOT "a.b.example.com". Set to 0 for unlimited depth.
type wildcardPattern struct {
	prefix            string
	suffix            string
	maxSubdomainDepth int
}

func (w wildcardPattern) match(origin string) bool {
	if len(origin) < len(w.prefix)+len(w.suffix)+1 {
		return false
	}
	if !strings.HasPrefix(origin, w.prefix) || !strings.HasSuffix(origin, w.suffix) {
		return false
	}
	if w.maxSubdomainDepth > 0 {
		middle := origin[len(w.prefix) : len(origin)-len(w.suffix)]
		dots := strings.Count(middle, ".")
		if dots >= w.maxSubdomainDepth {
			return false
		}
	}
	return true
}

// precomputed holds pre-joined header values for zero-alloc responses.
type precomputed struct {
	allowAllOrigins        bool
	nullAllowed            bool
	originSet              map[string]struct{}
	wildcardPatterns       []wildcardPattern
	allowOriginsFunc       func(string) bool
	allowOriginRequestFunc func(*celeris.Context, string) bool
	allowMethods           string
	allowHeaders           string
	mirrorRequestHeaders   bool
	exposeHeaders          string
	maxAge                 string
	preflightVary          string
	allowPrivateNetwork    bool
}

// isOriginAllowed checks whether origin is permitted by running the full
// matching cascade: allowAll -> exactMap -> null -> wildcardPatterns ->
// AllowOriginsFunc -> AllowOriginRequestFunc.
func (p *precomputed) isOriginAllowed(c *celeris.Context, origin string) bool {
	if p.allowAllOrigins {
		return true
	}

	normOrigin := normalizeOrigin(origin)

	if _, ok := p.originSet[normOrigin]; ok {
		return true
	}
	if origin == "null" && p.nullAllowed {
		return true
	}
	for _, wp := range p.wildcardPatterns {
		if wp.match(normOrigin) {
			return true
		}
	}
	if p.allowOriginsFunc != nil {
		if !isSerializedOrigin(origin) {
			return false
		}
		if p.allowOriginsFunc(origin) {
			return true
		}
	}
	if p.allowOriginRequestFunc != nil {
		if !isSerializedOrigin(origin) {
			return false
		}
		if p.allowOriginRequestFunc(c, origin) {
			return true
		}
	}
	return false
}

func precompute(cfg Config) precomputed {
	p := precomputed{
		allowMethods:           strings.Join(cfg.AllowMethods, ", "),
		allowOriginsFunc:       cfg.AllowOriginsFunc,
		allowOriginRequestFunc: cfg.AllowOriginRequestFunc,
		allowPrivateNetwork:    cfg.AllowPrivateNetwork,
	}

	if cfg.MirrorRequestHeaders {
		p.mirrorRequestHeaders = true
	} else {
		p.allowHeaders = strings.Join(cfg.AllowHeaders, ", ")
	}

	if len(cfg.ExposeHeaders) > 0 {
		p.exposeHeaders = strings.Join(cfg.ExposeHeaders, ", ")
	}
	if cfg.MaxAge < 0 {
		p.maxAge = "0"
	} else if cfg.MaxAge > 0 {
		p.maxAge = strconv.Itoa(cfg.MaxAge)
	}

	for _, o := range cfg.AllowOrigins {
		if o == "*" {
			p.allowAllOrigins = true
			break
		}
	}

	if !p.allowAllOrigins {
		p.originSet = make(map[string]struct{})
		for _, o := range cfg.AllowOrigins {
			if o == "null" {
				p.nullAllowed = true
				continue
			}
			if strings.Contains(o, "*") {
				if strings.Count(o, "*") > 1 {
					panic("cors: origin pattern must contain at most one wildcard: [redacted]")
				}
				norm := normalizeOrigin(o)
				idx := strings.Index(norm, "*")
				p.wildcardPatterns = append(p.wildcardPatterns, wildcardPattern{
					prefix:            norm[:idx],
					suffix:            norm[idx+1:],
					maxSubdomainDepth: 1,
				})
			} else {
				p.originSet[normalizeOrigin(o)] = struct{}{}
			}
		}
	}

	if p.allowAllOrigins {
		vary := "Access-Control-Request-Method, Access-Control-Request-Headers"
		if cfg.AllowPrivateNetwork {
			vary += ", Access-Control-Request-Private-Network"
		}
		p.preflightVary = vary
	} else {
		vary := "Origin, Access-Control-Request-Method, Access-Control-Request-Headers"
		if cfg.AllowPrivateNetwork {
			vary += ", Access-Control-Request-Private-Network"
		}
		p.preflightVary = vary
	}

	return p
}

// isSerializedOrigin checks whether an origin string looks like a valid
// serialized origin per RFC 6454: scheme "://" host [ ":" port ].
// Returns false for origins with path, query, fragment, or userinfo.
// Zero-alloc: uses byte scanning instead of url.Parse.
func isSerializedOrigin(origin string) bool {
	sep := strings.Index(origin, "://")
	if sep <= 0 {
		return false
	}
	authority := origin[sep+3:]
	if authority == "" {
		return false
	}
	for i := 0; i < len(authority); i++ {
		switch authority[i] {
		case '/', '?', '#', '@':
			return false
		}
	}
	return true
}

// normalizeOrigin lowercases the scheme and host of a URL-shaped origin.
// Plain values (e.g. "*") are returned unchanged. Returns "" for origins
// with a non-empty path, query, or fragment, which are invalid per RFC 6454.
// Zero-alloc: uses byte scanning instead of url.Parse.
func normalizeOrigin(raw string) string {
	sep := strings.Index(raw, "://")
	if sep < 0 {
		return raw
	}
	scheme := raw[:sep]
	rest := raw[sep+3:]
	if rest == "" {
		return raw
	}

	// Find end of authority: first '/', '?', or '#'.
	authEnd := len(rest)
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '/':
			// Allow a single trailing slash (strip it); reject non-empty path.
			if i == len(rest)-1 {
				authEnd = i
			} else {
				return ""
			}
		case '?', '#':
			return ""
		}
	}

	authority := rest[:authEnd]
	if authority == "" {
		return raw
	}

	return strings.ToLower(scheme) + "://" + strings.ToLower(authority)
}
