package pprof

import (
	"net/http/pprof"
	"strings"

	"github.com/goceleris/celeris"
)

// Pre-wrap all standard net/http/pprof handlers via celeris.Adapt at init
// time so no allocation happens per-request.
var (
	adaptedIndex        = celeris.AdaptFunc(pprof.Index)
	adaptedCmdline      = celeris.AdaptFunc(pprof.Cmdline)
	adaptedProfile      = celeris.AdaptFunc(pprof.Profile)
	adaptedSymbol       = celeris.AdaptFunc(pprof.Symbol)
	adaptedTrace        = celeris.AdaptFunc(pprof.Trace)
	adaptedAllocs       = celeris.Adapt(pprof.Handler("allocs"))
	adaptedBlock        = celeris.Adapt(pprof.Handler("block"))
	adaptedGoroutine    = celeris.Adapt(pprof.Handler("goroutine"))
	adaptedHeap         = celeris.Adapt(pprof.Handler("heap"))
	adaptedMutex        = celeris.Adapt(pprof.Handler("mutex"))
	adaptedThreadcreate = celeris.Adapt(pprof.Handler("threadcreate"))
)

// New creates a pprof middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	var skip celeris.SkipHelper
	skip.Init(cfg.SkipPaths, cfg.Skip)

	prefix := strings.TrimRight(cfg.Prefix, "/")
	prefixSlash := prefix + "/"

	indexPath := prefix + "/index"
	cmdlinePath := prefix + "/cmdline"
	profilePath := prefix + "/profile"
	symbolPath := prefix + "/symbol"
	tracePath := prefix + "/trace"
	allocsPath := prefix + "/allocs"
	blockPath := prefix + "/block"
	goroutinePath := prefix + "/goroutine"
	heapPath := prefix + "/heap"
	mutexPath := prefix + "/mutex"
	threadcreatePath := prefix + "/threadcreate"

	authFunc := cfg.AuthFunc

	return func(c *celeris.Context) error {
		path := c.Path()

		// Fast path: requests outside the prefix pass through immediately.
		if path != prefix && !strings.HasPrefix(path, prefixSlash) {
			return c.Next()
		}

		if skip.ShouldSkip(c) {
			return c.Next()
		}

		if authFunc != nil && !authFunc(c) {
			return c.NoContent(403)
		}

		switch path {
		case prefix, prefixSlash:
			return adaptedIndex(c)
		case indexPath:
			return adaptedIndex(c)
		case cmdlinePath:
			return adaptedCmdline(c)
		case profilePath:
			return adaptedProfile(c)
		case symbolPath:
			return adaptedSymbol(c)
		case tracePath:
			return adaptedTrace(c)
		case allocsPath:
			return adaptedAllocs(c)
		case blockPath:
			return adaptedBlock(c)
		case goroutinePath:
			return adaptedGoroutine(c)
		case heapPath:
			return adaptedHeap(c)
		case mutexPath:
			return adaptedMutex(c)
		case threadcreatePath:
			return adaptedThreadcreate(c)
		default:
			return c.NoContent(404)
		}
	}
}
