package debug

import (
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/observe"
)

type memStatsCache struct {
	mu       sync.Mutex
	data     memoryResponse
	cachedAt time.Time
	ttl      time.Duration
}

type statusResponse struct {
	// Uptime is the elapsed time since the middleware was initialized, formatted as a Go duration string.
	Uptime string `json:"uptime"`
	// GoVersion is the Go toolchain version (e.g. "go1.26").
	GoVersion string `json:"go_version"`
}

type memoryResponse struct {
	// Alloc is the current heap allocation in bytes.
	Alloc uint64 `json:"alloc"`
	// TotalAlloc is the cumulative bytes allocated over the process lifetime (never decreases).
	TotalAlloc uint64 `json:"total_alloc"`
	// Sys is the total bytes of memory obtained from the OS.
	Sys uint64 `json:"sys"`
	// HeapInuse is the bytes in in-use heap spans.
	HeapInuse uint64 `json:"heap_inuse"`
	// HeapIdle is the bytes in idle (unused) heap spans.
	HeapIdle uint64 `json:"heap_idle"`
	// NumGC is the number of completed GC cycles.
	NumGC uint32 `json:"num_gc"`
	// GCCPUFrac is the fraction of CPU time spent in GC since the program started (0.0 to 1.0).
	GCCPUFrac float64 `json:"gc_cpu_fraction"`
}

type buildResponse struct {
	// Module is the main module path from build info (e.g. "github.com/goceleris/celeris").
	Module string `json:"module"`
	// GoVersion is the Go toolchain version used to build the binary.
	GoVersion string `json:"go_version"`
	// VCS contains version-control metadata (e.g. "vcs.revision", "vcs.time", "vcs.modified").
	VCS map[string]string `json:"vcs,omitempty"`
}

type runtimeResponse struct {
	// Goroutines is the current number of goroutines.
	Goroutines int `json:"goroutines"`
	// NumCPU is the number of logical CPUs available to the process.
	NumCPU int `json:"num_cpu"`
	// GOMAXPROCS is the current value of GOMAXPROCS (max concurrently executing goroutines).
	GOMAXPROCS int `json:"gomaxprocs"`
}

// allEndpoints is the canonical list of endpoint names.
var allEndpoints = [...]string{"status", "metrics", "config", "routes", "memory", "build", "runtime"}

func isEndpointEnabled(endpoints map[string]bool, name string) bool {
	if endpoints == nil {
		return true
	}
	enabled, ok := endpoints[name]
	return ok && enabled
}

// New creates a debug middleware with the given config.
func New(config ...Config) celeris.HandlerFunc {
	cfg := defaultConfigCopy()
	if len(config) > 0 {
		cfg = config[0]
	}
	cfg = applyDefaults(cfg)
	cfg.validate()

	skipMap := make(map[string]struct{}, len(cfg.SkipPaths))
	for _, p := range cfg.SkipPaths {
		skipMap[p] = struct{}{}
	}

	prefix := strings.TrimRight(cfg.Prefix, "/")
	prefixSlash := prefix + "/"
	startTime := time.Now()

	statusPath := prefix + "/status"
	metricsPath := prefix + "/metrics"
	configPath := prefix + "/config"
	routesPath := prefix + "/routes"
	memoryPath := prefix + "/memory"
	buildPath := prefix + "/build"
	runtimePath := prefix + "/runtime"

	endpoints := cfg.Endpoints
	msCache := &memStatsCache{ttl: cfg.MemStatsTTL}

	// Pre-build the index listing of available endpoints.
	available := make([]string, 0, len(allEndpoints))
	for _, name := range allEndpoints {
		if isEndpointEnabled(endpoints, name) {
			available = append(available, prefixSlash+name)
		}
	}

	return func(c *celeris.Context) error {
		path := c.Path()

		if _, ok := skipMap[path]; ok {
			return c.Next()
		}

		// Check prefix before Skip so the Skip function is only consulted
		// for requests that actually target a debug endpoint.
		if path != prefix && !strings.HasPrefix(path, prefixSlash) {
			return c.Next()
		}

		if cfg.Skip != nil && cfg.Skip(c) {
			return c.Next()
		}

		if path == prefix || path == prefixSlash {
			if cfg.AuthFunc != nil && !cfg.AuthFunc(c) {
				return c.NoContent(403)
			}
			method := c.Method()
			if method != "GET" && method != "HEAD" {
				return c.NoContent(405)
			}
			return c.JSON(200, available)
		}

		if cfg.AuthFunc != nil && !cfg.AuthFunc(c) {
			return c.NoContent(403)
		}

		method := c.Method()
		if method != "GET" && method != "HEAD" {
			return c.NoContent(405)
		}

		switch path {
		case statusPath:
			if !isEndpointEnabled(endpoints, "status") {
				return c.NoContent(404)
			}
			return handleStatus(c, startTime)
		case metricsPath:
			if !isEndpointEnabled(endpoints, "metrics") {
				return c.NoContent(404)
			}
			return handleMetrics(c, cfg.Collector)
		case configPath:
			if !isEndpointEnabled(endpoints, "config") {
				return c.NoContent(404)
			}
			return handleConfig(c)
		case routesPath:
			if !isEndpointEnabled(endpoints, "routes") {
				return c.NoContent(404)
			}
			return handleRoutes(c, cfg.Server)
		case memoryPath:
			if !isEndpointEnabled(endpoints, "memory") {
				return c.NoContent(404)
			}
			return handleMemory(c, msCache)
		case buildPath:
			if !isEndpointEnabled(endpoints, "build") {
				return c.NoContent(404)
			}
			return handleBuild(c)
		case runtimePath:
			if !isEndpointEnabled(endpoints, "runtime") {
				return c.NoContent(404)
			}
			return handleRuntime(c)
		default:
			return c.NoContent(404)
		}
	}
}

func handleStatus(c *celeris.Context, startTime time.Time) error {
	return c.JSON(200, statusResponse{
		Uptime:    time.Since(startTime).String(),
		GoVersion: runtime.Version(),
	})
}

func handleMetrics(c *celeris.Context, collector *observe.Collector) error {
	if collector == nil {
		return c.NoContent(501)
	}
	return c.JSON(200, collector.Snapshot())
}

type configResponse struct {
	// GoVersion is the Go toolchain version (e.g. "go1.26").
	GoVersion string `json:"go_version"`
	// GoOS is the target operating system (e.g. "linux", "darwin").
	GoOS string `json:"go_os"`
	// GoArch is the target architecture (e.g. "amd64", "arm64").
	GoArch string `json:"go_arch"`
	// NumCPU is the number of logical CPUs available to the process.
	NumCPU int `json:"num_cpu"`
	// Goroutines is the current number of goroutines at the time of the request.
	Goroutines int `json:"goroutines"`
}

var cachedConfig = configResponse{
	GoVersion: runtime.Version(),
	GoOS:      runtime.GOOS,
	GoArch:    runtime.GOARCH,
	NumCPU:    runtime.NumCPU(),
}

func handleConfig(c *celeris.Context) error {
	resp := cachedConfig
	resp.Goroutines = runtime.NumGoroutine()
	return c.JSON(200, resp)
}

func handleRoutes(c *celeris.Context, server *celeris.Server) error {
	if server == nil {
		return c.JSON(200, []any{})
	}
	return c.JSON(200, server.Routes())
}

func handleMemory(c *celeris.Context, cache *memStatsCache) error {
	// Check cache under lock (fast path).
	cache.mu.Lock()
	if cache.ttl > 0 && time.Since(cache.cachedAt) < cache.ttl {
		resp := cache.data
		cache.mu.Unlock()
		return c.JSON(200, resp)
	}
	cache.mu.Unlock()

	// Read memstats outside the lock to avoid holding the mutex across
	// the STW pause that ReadMemStats triggers.
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	resp := memoryResponse{
		Alloc:      m.Alloc,
		TotalAlloc: m.TotalAlloc,
		Sys:        m.Sys,
		HeapInuse:  m.HeapInuse,
		HeapIdle:   m.HeapIdle,
		NumGC:      m.NumGC,
		GCCPUFrac:  m.GCCPUFraction,
	}

	// Re-acquire to store the fresh data.
	cache.mu.Lock()
	cache.data = resp
	cache.cachedAt = time.Now()
	cache.mu.Unlock()

	return c.JSON(200, resp)
}

func handleRuntime(c *celeris.Context) error {
	return c.JSON(200, runtimeResponse{
		Goroutines: runtime.NumGoroutine(),
		NumCPU:     runtime.NumCPU(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
	})
}

// cachedBuild is computed once at init time since build info never changes.
var cachedBuild = func() buildResponse {
	resp := buildResponse{
		GoVersion: runtime.Version(),
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		resp.Module = info.Main.Path
		vcs := make(map[string]string)
		for _, s := range info.Settings {
			if strings.HasPrefix(s.Key, "vcs.") {
				vcs[s.Key] = s.Value
			}
		}
		if len(vcs) > 0 {
			resp.VCS = vcs
		}
	}
	return resp
}()

func handleBuild(c *celeris.Context) error {
	return c.JSON(200, cachedBuild)
}
