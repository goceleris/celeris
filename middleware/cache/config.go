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

	// Singleflight coalesces concurrent cache-miss requests for the same
	// key so only one handler invocation runs; waiters reuse the
	// resulting response. Default: true.
	Singleflight bool

	// Methods lists HTTP methods eligible for caching. Default: GET, HEAD.
	// Methods not in this list pass through without interacting with
	// the cache.
	Methods []string

	// StatusFilter decides whether a computed response should be stored.
	// When nil, only 2xx responses are cached.
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

	// RespectCacheControl, when true (default), honors the response's
	// Cache-Control directive:
	//   - no-store or private → skip caching
	//   - max-age=N           → cap TTL to min(cfg.TTL, N)
	RespectCacheControl bool

	// Skip defines a function to skip this middleware for certain
	// requests.
	Skip func(*celeris.Context) bool

	// SkipPaths lists paths to skip (exact match).
	SkipPaths []string
}

var defaultConfig = Config{
	TTL:                 time.Minute,
	Singleflight:        true,
	Methods:             []string{"GET", "HEAD"},
	HeaderName:          "X-Cache",
	MaxBodyBytes:        1 << 20,
	RespectCacheControl: true,
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
	// RespectCacheControl defaults to true; preserve explicit false.
	// Since the zero value is false, we default it to true only when
	// the entire Config is zero-valued (nothing else set).
	if !cfg.RespectCacheControl && !cfg.Singleflight &&
		cfg.Store == nil && cfg.TTL == 0 {
		cfg.RespectCacheControl = true
	} else if !cfg.RespectCacheControl {
		cfg.RespectCacheControl = true
	}
	return cfg
}

func defaultStatusFilter(status int) bool {
	return status >= 200 && status < 300
}
