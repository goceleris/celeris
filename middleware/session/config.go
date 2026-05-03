package session

import (
	"strings"
	"time"

	"github.com/goceleris/celeris"

	"github.com/goceleris/celeris/middleware/internal/randutil"
	"github.com/goceleris/celeris/middleware/store"
)

// ContextKey is the context store key for the session object.
const ContextKey = "session"

// absExpKey is the reserved session data key for absolute expiry timestamp.
const absExpKey = "_abs_exp"

// Extractor is a function that extracts the session ID from a request.
// The default extractor reads the session cookie. For API clients, use
// [HeaderExtractor] or [QueryExtractor] instead.
type Extractor func(c *celeris.Context) string

// CookieExtractor returns an [Extractor] that reads the session ID from
// the named cookie.
func CookieExtractor(cookieName string) Extractor {
	return func(c *celeris.Context) string {
		v, err := c.Cookie(cookieName)
		if err != nil {
			return ""
		}
		return v
	}
}

// HeaderExtractor returns an [Extractor] that reads the session ID from
// the named request header. Use this for API clients that cannot store
// cookies. Header name is pre-lowercased so c.Header's own fast-path
// fires without allocating per request.
func HeaderExtractor(headerName string) Extractor {
	h := strings.ToLower(headerName)
	return func(c *celeris.Context) string {
		return c.Header(h)
	}
}

// QueryExtractor returns an [Extractor] that reads the session ID from
// the named query parameter.
func QueryExtractor(paramName string) Extractor {
	return func(c *celeris.Context) string {
		return c.Query(paramName)
	}
}

// Config defines the session middleware configuration.
type Config struct {
	// Skip defines a function to skip this middleware for certain requests.
	Skip func(c *celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string

	// Store is the session backend. Default: [NewMemoryStore].
	// Sessions are JSON-encoded before persistence; custom [store.KV]
	// backends (Redis, Postgres) must preserve byte slice contents.
	Store store.KV

	// Extractor extracts the session ID from the request. Default:
	// CookieExtractor(CookieName). When a non-cookie extractor is used,
	// the session ID is also set in a response header named after the
	// cookie name for API clients.
	Extractor Extractor

	// CookieName is the session cookie name. Default: "celeris_session".
	CookieName string

	// CookiePath is the cookie Path attribute. Default: "/".
	CookiePath string

	// CookieDomain is the cookie Domain attribute.
	CookieDomain string

	// CookieMaxAge is the cookie Max-Age in seconds.
	// nil means "use default (86400 = 24h)". A non-nil pointer selects
	// the exact value: 0 for a session cookie (no Max-Age attribute),
	// positive for an explicit lifetime.
	//
	// Note: CookieMaxAge controls browser-side cookie lifetime and is
	// independent of IdleTimeout (server-side store expiry). If CookieMaxAge
	// is shorter than IdleTimeout, the browser will discard the cookie while
	// the server still holds the session data. If longer, the browser may
	// send a cookie for a session the server has already evicted. For
	// consistent behavior, align CookieMaxAge with IdleTimeout (e.g.,
	// CookieMaxAge = intPtr(int(IdleTimeout.Seconds()))).
	CookieMaxAge *int

	// CookieSecure flags the cookie for HTTPS-only transmission.
	CookieSecure bool

	// CookieHTTPOnly prevents client-side scripts from accessing the cookie.
	// nil means "use default (true)". Set to a non-nil pointer to
	// explicitly enable or disable: BoolPtr(false) disables HTTPOnly
	// for JS-based session management.
	CookieHTTPOnly *bool

	// CookieSameSite controls cross-site request cookie behavior.
	// Default: celeris.SameSiteLaxMode.
	CookieSameSite celeris.SameSite

	// IdleTimeout is how long a session can be idle before expiring.
	// Default: 30 minutes.
	IdleTimeout time.Duration

	// AbsoluteTimeout is the maximum lifetime of a session regardless of
	// activity. Default: 24 hours. Set to 0 for the default, or -1 to
	// disable absolute timeout enforcement entirely.
	AbsoluteTimeout time.Duration

	// KeyGenerator generates new session IDs. Default: 32-byte crypto/rand hex.
	KeyGenerator func() string

	// ErrorHandler is called when a store operation fails. If nil, the error
	// is returned up the middleware chain.
	ErrorHandler func(c *celeris.Context, err error) error
}

// defaultCookieMaxAge is the default cookie Max-Age in seconds (24h).
var defaultCookieMaxAge = 86400

// defaultCookieHTTPOnly is the default for the HTTPOnly cookie attribute.
var defaultCookieHTTPOnly = true

var defaultConfig = Config{
	CookieName:      "celeris_session",
	CookiePath:      "/",
	CookieMaxAge:    &defaultCookieMaxAge,
	CookieHTTPOnly:  &defaultCookieHTTPOnly,
	CookieSameSite:  celeris.SameSiteLaxMode,
	IdleTimeout:     30 * time.Minute,
	AbsoluteTimeout: 24 * time.Hour,
}

// BoolPtr returns a pointer to the given bool value. Use this to
// explicitly set [Config].CookieHTTPOnly:
//
//	session.New(session.Config{CookieHTTPOnly: session.BoolPtr(false)})
func BoolPtr(v bool) *bool { return &v }

// IntPtr returns a pointer to the given int value. Use this to
// explicitly set [Config].CookieMaxAge:
//
//	session.New(session.Config{CookieMaxAge: session.IntPtr(0)}) // session cookie
func IntPtr(v int) *int { return &v }

// ChainExtractor returns an [Extractor] that tries each extractor in order
// and returns the first non-empty result. This is useful when session IDs
// may arrive via different transport mechanisms (e.g., cookie for browsers,
// header for API clients).
func ChainExtractor(extractors ...Extractor) Extractor {
	return func(c *celeris.Context) string {
		for _, e := range extractors {
			if sid := e(c); sid != "" {
				return sid
			}
		}
		return ""
	}
}

func applyDefaults(cfg Config) Config {
	// When no store is provided, applyDefaults creates a MemoryStore which
	// spawns a background cleanup goroutine. This is intentional: the
	// middleware is typically created once at startup and the goroutine
	// runs for the process lifetime. Use [MemoryStoreConfig].CleanupContext
	// or the [memoryStore.Close] method for deterministic shutdown.
	if cfg.Store == nil {
		cfg.Store = NewMemoryStore()
	}
	if cfg.CookieName == "" {
		cfg.CookieName = defaultConfig.CookieName
	}
	if cfg.CookiePath == "" {
		cfg.CookiePath = defaultConfig.CookiePath
	}
	if cfg.CookieMaxAge == nil {
		cfg.CookieMaxAge = defaultConfig.CookieMaxAge
	}
	if cfg.CookieSameSite == 0 {
		cfg.CookieSameSite = defaultConfig.CookieSameSite
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultConfig.IdleTimeout
	}
	// AbsoluteTimeout: 0 → default (24h), -1 → disabled, >0 → as-is.
	if cfg.AbsoluteTimeout == 0 {
		cfg.AbsoluteTimeout = defaultConfig.AbsoluteTimeout
	}
	if cfg.KeyGenerator == nil {
		cfg.KeyGenerator = func() string { return randutil.HexToken(keyBytes) }
	}
	if cfg.CookieHTTPOnly == nil {
		cfg.CookieHTTPOnly = defaultConfig.CookieHTTPOnly
	}
	return cfg
}

func (cfg Config) validate() {
	if cfg.AbsoluteTimeout > 0 && cfg.AbsoluteTimeout < cfg.IdleTimeout {
		panic("session: AbsoluteTimeout must be >= IdleTimeout")
	}
	if cfg.CookieSameSite == celeris.SameSiteNoneMode && !cfg.CookieSecure {
		panic("session: CookieSecure must be true when CookieSameSite is None")
	}
}

const keyBytes = 32
const sessionIDHexLen = keyBytes * 2

// newBufferedKeyGenerator returns a key generator backed by the shared
// randutil buffered generator. Retained for test compatibility.
func newBufferedKeyGenerator() *bufferedKeyGen {
	return &bufferedKeyGen{}
}

type bufferedKeyGen struct{}

func (g *bufferedKeyGen) generate() string {
	return randutil.HexToken(keyBytes)
}
