package cache

import (
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/store"
)

// Config defines the cache middleware configuration.
type Config struct {
	// Store is the cache backend. Default: [NewMemoryStore].
	Store store.KV

	// TTL is the default cache entry lifetime. Default: 1 minute.
	// Per-response Cache-Control: max-age directives, when respected,
	// cap the effective TTL to min(TTL, max-age).
	TTL time.Duration

	// KeyGenerator derives the cache key from a request. When nil, the
	// default uses method + path + sorted query string + values of every
	// header listed in [VaryHeaders].
	KeyGenerator func(*celeris.Context) string

	// DisableSingleflight turns off the coalescing of concurrent cache
	// misses. By default (false) concurrent requests that miss on the same
	// key coalesce: only one handler invocation runs, and the waiters
	// reuse the resulting response. When the handler returns an error,
	// each waiter returns a copy of it made before the leader returns, as
	// middleware/singleflight does: its message is copied, it unwraps to
	// the leader's error ([errors.Is] and [errors.As] find what it holds),
	// and it is not == to it. When the handler (or the store's Set)
	// panics, the panic is the leader's, and each waiter runs its own
	// handler. A waiter waits only as long as its request context lives.
	//
	// It replaces the Singleflight field, which a Config literal that did
	// not set it turned off (celeris#922).
	DisableSingleflight bool

	// Methods lists HTTP methods eligible for caching. Default: GET, HEAD.
	// Methods not in this list pass through without interacting with
	// the cache.
	Methods []string

	// StatusFilter decides whether a computed response should be stored.
	// When nil, only 2xx responses are cached. A 206 Partial Content or a
	// 416 Range Not Satisfiable is never stored, whatever the filter says,
	// even when the key includes Range (in VaryHeaders or a KeyGenerator):
	// both answer one request's Range, and a replay would skip the
	// handler's If-Range check, which can make the same Range get the whole
	// representation.
	StatusFilter func(status int) bool

	// VaryHeaders are included in the default cache key. Callers who
	// provide their own [KeyGenerator] can ignore this field.
	VaryHeaders []string

	// HeaderName is the response header populated with "HIT" or "MISS".
	// Default: "X-Cache". Set to "" to disable.
	HeaderName string

	// MaxBodyBytes caps the response body size eligible for caching.
	// Responses larger than this pass through uncached. Default: 1 MiB.
	MaxBodyBytes int

	// IncludeHeaders, when non-empty, whitelists response headers that
	// are stored alongside the body. When nil, all response headers
	// are stored except those in [ExcludeHeaders] (default: Set-Cookie).
	IncludeHeaders []string

	// ExcludeHeaders, when non-empty, is subtracted from the stored
	// header set after applying [IncludeHeaders]. Default: "set-cookie".
	ExcludeHeaders []string

	// IgnoreCacheControl, when true, stores responses whatever their
	// Cache-Control says. By default (false) the response's Cache-Control
	// directive is honoured:
	//   - no-store or private → skip caching
	//   - max-age=N           → cap TTL to min(cfg.TTL, N)
	//
	// It replaces the RespectCacheControl field, whose false had no effect
	// (celeris#922).
	IgnoreCacheControl bool

	// Skip defines a function to skip this middleware for certain
	// requests.
	Skip func(*celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string
}

// defaultConfig holds the defaults. Every bool field's default is false, so
// a Config literal that leaves one out gets its default (celeris#922).
var defaultConfig = Config{
	TTL:          time.Minute,
	Methods:      []string{"GET", "HEAD"},
	HeaderName:   "X-Cache",
	MaxBodyBytes: 1 << 20,
}

func applyDefaults(cfg Config) Config {
	if cfg.Store == nil {
		cfg.Store = NewMemoryStore()
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultConfig.TTL
	}
	if cfg.Methods == nil {
		cfg.Methods = defaultConfig.Methods
	}
	if cfg.HeaderName == "" {
		cfg.HeaderName = defaultConfig.HeaderName
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = defaultConfig.MaxBodyBytes
	}
	if cfg.StatusFilter == nil {
		cfg.StatusFilter = defaultStatusFilter
	}
	if cfg.ExcludeHeaders == nil {
		cfg.ExcludeHeaders = []string{"set-cookie"}
	}
	return cfg
}

func defaultStatusFilter(status int) bool {
	return status >= 200 && status < 300
}
