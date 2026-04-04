package debug

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"
	"github.com/goceleris/celeris/observe"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

// openAuth allows all requests (overrides default localhost-only).
func openAuth() func(*celeris.Context) bool {
	return func(_ *celeris.Context) bool { return true }
}

func okHandler(c *celeris.Context) error {
	return c.String(200, "ok")
}

func TestStatusEndpoint(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/status")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var resp statusResponse
	if err := json.Unmarshal(rec.Body, &resp); err != nil {
		t.Fatalf("failed to unmarshal status response: %v", err)
	}
	if resp.GoVersion != runtime.Version() {
		t.Fatalf("go_version: got %q, want %q", resp.GoVersion, runtime.Version())
	}
	if resp.Uptime == "" {
		t.Fatal("expected non-empty uptime")
	}
}

func TestMetricsEndpointWithCollector(t *testing.T) {
	col := observe.NewCollector()
	mw := New(Config{Collector: col, AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/metrics")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var snap observe.Snapshot
	if err := json.Unmarshal(rec.Body, &snap); err != nil {
		t.Fatalf("failed to unmarshal snapshot: %v", err)
	}
	if snap.BucketBounds == nil {
		t.Fatal("expected non-nil BucketBounds")
	}
}

func TestMetricsEndpointWithoutCollector(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/metrics")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 501)
	testutil.AssertBodyEmpty(t, rec)
}

func TestConfigEndpoint(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/config")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var resp map[string]any
	if err := json.Unmarshal(rec.Body, &resp); err != nil {
		t.Fatalf("failed to unmarshal config response: %v", err)
	}
	if resp["go_version"] != runtime.Version() {
		t.Fatalf("go_version: got %v, want %q", resp["go_version"], runtime.Version())
	}
	if resp["go_os"] != runtime.GOOS {
		t.Fatalf("go_os: got %v, want %q", resp["go_os"], runtime.GOOS)
	}
	if resp["go_arch"] != runtime.GOARCH {
		t.Fatalf("go_arch: got %v, want %q", resp["go_arch"], runtime.GOARCH)
	}
	if resp["num_cpu"] == nil {
		t.Fatal("expected num_cpu in response")
	}
	if resp["goroutines"] == nil {
		t.Fatal("expected goroutines in response")
	}
}

func TestRoutesEndpointWithoutServer(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/routes")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "[]")
}

func TestRoutesEndpointWithServer(t *testing.T) {
	server := celeris.New(celeris.Config{})
	server.GET("/api/users", func(c *celeris.Context) error {
		return c.String(200, "users")
	})
	server.POST("/api/users", func(c *celeris.Context) error {
		return c.String(201, "created")
	})

	mw := New(Config{Server: server, AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/routes")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var routes []map[string]any
	if err := json.Unmarshal(rec.Body, &routes); err != nil {
		t.Fatalf("failed to unmarshal routes: %v", err)
	}
	if len(routes) < 2 {
		t.Fatalf("expected at least 2 routes, got %d", len(routes))
	}

	foundGet := false
	foundPost := false
	for _, r := range routes {
		method, _ := r["Method"].(string)
		path, _ := r["Path"].(string)
		if method == "GET" && path == "/api/users" {
			foundGet = true
		}
		if method == "POST" && path == "/api/users" {
			foundPost = true
		}
	}
	if !foundGet {
		t.Fatal("expected GET /api/users in routes")
	}
	if !foundPost {
		t.Fatal("expected POST /api/users in routes")
	}
}

func TestAuthFuncBlocks(t *testing.T) {
	mw := New(Config{
		AuthFunc: func(_ *celeris.Context) bool { return false },
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/status")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 403)
	testutil.AssertBodyEmpty(t, rec)
}

func TestAuthFuncAllows(t *testing.T) {
	mw := New(Config{
		AuthFunc: func(_ *celeris.Context) bool { return true },
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/status")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "go_version")
}

func TestAuthFuncChecksHeader(t *testing.T) {
	mw := New(Config{
		AuthFunc: func(c *celeris.Context) bool {
			return c.Header("x-debug-token") == "secret"
		},
	})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/status")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 403)

	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/status",
		celeristest.WithHeader("x-debug-token", "secret"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestDefaultAuthBlocksNonLoopback(t *testing.T) {
	mw := New()
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/status",
		celeristest.WithRemoteAddr("192.168.1.100:12345"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 403)
}

func TestDefaultAuthAllowsLoopback(t *testing.T) {
	mw := New()
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/status",
		celeristest.WithRemoteAddr("127.0.0.1:12345"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestNonDebugPathPassesThrough(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/api/users")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestCustomPrefix(t *testing.T) {
	mw := New(Config{Prefix: "/_internal", AuthFunc: openAuth()})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/_internal/status")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "go_version")

	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err = testutil.RunChain(t, chain, "GET", "/debug/celeris/status")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestCustomPrefixTrailingSlash(t *testing.T) {
	mw := New(Config{Prefix: "/_debug/", AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/_debug/status")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "go_version")
}

func TestUnknownDebugSubpath(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/unknown")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 404)
	testutil.AssertBodyEmpty(t, rec)
}

func TestDefaultConfigPrefix(t *testing.T) {
	cfg := defaultConfigCopy()
	if cfg.Prefix != "/debug/celeris" {
		t.Fatalf("defaultConfigCopy().Prefix: got %q, want %q", cfg.Prefix, "/debug/celeris")
	}
}

func TestIPv6Loopback(t *testing.T) {
	mw := New()
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/status",
		celeristest.WithRemoteAddr("[::1]:12345"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func FuzzDebugPaths(f *testing.F) {
	f.Add("/debug/celeris/status")
	f.Add("/debug/celeris/metrics")
	f.Add("/debug/celeris/config")
	f.Add("/debug/celeris/routes")
	f.Add("/debug/celeris/memory")
	f.Add("/debug/celeris/build")
	f.Add("/debug/celeris/unknown")
	f.Add("/debug/celeris/")
	f.Add("/api/users")
	f.Add("")
	f.Fuzz(func(t *testing.T, path string) {
		mw := New(Config{AuthFunc: func(_ *celeris.Context) bool { return true }})
		ctx, _ := celeristest.NewContextT(t, "GET", path)
		_ = mw(ctx)
	})
}

func TestDefaultConfigAuthFunc(t *testing.T) {
	cfg := defaultConfigCopy()
	if cfg.AuthFunc == nil {
		t.Fatal("defaultConfigCopy().AuthFunc should not be nil")
	}
}

func TestApplyDefaultsFillsPrefix(t *testing.T) {
	cfg := applyDefaults(Config{})
	if cfg.Prefix != "/debug/celeris" {
		t.Fatalf("applyDefaults Prefix: got %q, want %q", cfg.Prefix, "/debug/celeris")
	}
}

func TestApplyDefaultsPreservesCustomPrefix(t *testing.T) {
	cfg := applyDefaults(Config{Prefix: "/custom"})
	if cfg.Prefix != "/custom" {
		t.Fatalf("applyDefaults Prefix: got %q, want %q", cfg.Prefix, "/custom")
	}
}

func TestPrefixOnlyServesIndex(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var endpoints []string
	if err := json.Unmarshal(rec.Body, &endpoints); err != nil {
		t.Fatalf("failed to unmarshal index response: %v", err)
	}
	if len(endpoints) != 7 {
		t.Fatalf("expected 7 endpoints in index, got %d", len(endpoints))
	}
}

func TestMetricsAfterRecording(t *testing.T) {
	col := observe.NewCollector()
	col.RecordRequest(1000, 200)
	col.RecordRequest(2000, 500)

	mw := New(Config{Collector: col, AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/metrics")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var snap observe.Snapshot
	if err := json.Unmarshal(rec.Body, &snap); err != nil {
		t.Fatalf("failed to unmarshal snapshot: %v", err)
	}
	if snap.RequestsTotal != 2 {
		t.Fatalf("RequestsTotal: got %d, want 2", snap.RequestsTotal)
	}
	if snap.ErrorsTotal != 1 {
		t.Fatalf("ErrorsTotal: got %d, want 1", snap.ErrorsTotal)
	}
}

func TestMemoryEndpoint(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/memory")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var resp memoryResponse
	if err := json.Unmarshal(rec.Body, &resp); err != nil {
		t.Fatalf("failed to unmarshal memory response: %v", err)
	}
	if resp.Alloc == 0 {
		t.Fatal("expected non-zero alloc")
	}
	if resp.Sys == 0 {
		t.Fatal("expected non-zero sys")
	}
	if resp.TotalAlloc == 0 {
		t.Fatal("expected non-zero total_alloc")
	}
}

func TestBuildEndpoint(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/build")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var resp buildResponse
	if err := json.Unmarshal(rec.Body, &resp); err != nil {
		t.Fatalf("failed to unmarshal build response: %v", err)
	}
	if resp.GoVersion != runtime.Version() {
		t.Fatalf("go_version: got %q, want %q", resp.GoVersion, runtime.Version())
	}
}

func TestSkipBypassesDebug(t *testing.T) {
	mw := New(Config{
		AuthFunc: openAuth(),
		Skip:     func(_ *celeris.Context) bool { return true },
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/debug/celeris/status")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestSkipNotCalledForNonDebugPaths(t *testing.T) {
	called := false
	mw := New(Config{
		AuthFunc: openAuth(),
		Skip: func(_ *celeris.Context) bool {
			called = true
			return false
		},
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/api/users")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
	if called {
		t.Fatal("Skip should not be called for non-debug paths")
	}
}

func TestEndpointsFilterDisable(t *testing.T) {
	mw := New(Config{
		AuthFunc: openAuth(),
		Endpoints: map[string]bool{
			"status":  true,
			"metrics": false,
			"config":  true,
			"routes":  true,
			"memory":  true,
			"build":   true,
		},
	})
	// metrics should be 404 when disabled.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/metrics")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 404)

	// status should still work.
	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/status")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestEndpointsFilterNilAllEnabled(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})

	for _, ep := range []string{"status", "metrics", "config", "routes", "memory", "build", "runtime"} {
		t.Run(ep, func(t *testing.T) {
			rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/"+ep)
			testutil.AssertNoError(t, err)
			if rec.StatusCode == 404 {
				t.Fatalf("endpoint %q should be enabled with nil Endpoints", ep)
			}
		})
	}
}

func TestIndexEndpoint(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var endpoints []string
	if err := json.Unmarshal(rec.Body, &endpoints); err != nil {
		t.Fatalf("failed to unmarshal index response: %v", err)
	}
	if len(endpoints) != 7 {
		t.Fatalf("expected 7 endpoints, got %d: %v", len(endpoints), endpoints)
	}
}

func TestIndexEndpointTrailingSlash(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var endpoints []string
	if err := json.Unmarshal(rec.Body, &endpoints); err != nil {
		t.Fatalf("failed to unmarshal index response: %v", err)
	}
	if len(endpoints) != 7 {
		t.Fatalf("expected 7 endpoints, got %d: %v", len(endpoints), endpoints)
	}
}

func TestIndexEndpointRespectsFilter(t *testing.T) {
	mw := New(Config{
		AuthFunc: openAuth(),
		Endpoints: map[string]bool{
			"status": true,
			"build":  true,
		},
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var endpoints []string
	if err := json.Unmarshal(rec.Body, &endpoints); err != nil {
		t.Fatalf("failed to unmarshal index response: %v", err)
	}
	if len(endpoints) != 2 {
		t.Fatalf("expected 2 endpoints, got %d: %v", len(endpoints), endpoints)
	}
}

func TestMemStatsTTLCaching(t *testing.T) {
	mw := New(Config{
		AuthFunc:    openAuth(),
		MemStatsTTL: 5 * time.Second,
	})

	// First request populates the cache.
	rec1, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/memory")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec1, 200)

	var resp1 memoryResponse
	if err := json.Unmarshal(rec1.Body, &resp1); err != nil {
		t.Fatalf("failed to unmarshal first response: %v", err)
	}

	// Second request immediately after should return cached data (identical).
	rec2, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/memory")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec2, 200)

	var resp2 memoryResponse
	if err := json.Unmarshal(rec2.Body, &resp2); err != nil {
		t.Fatalf("failed to unmarshal second response: %v", err)
	}

	// Cached responses must be identical: same NumGC and TotalAlloc prove
	// the second call did not re-read MemStats.
	if resp1.NumGC != resp2.NumGC || resp1.TotalAlloc != resp2.TotalAlloc {
		t.Fatalf("expected cached response, got different values:\n  first:  %+v\n  second: %+v", resp1, resp2)
	}
}

func TestRuntimeEndpoint(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/runtime")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var resp runtimeResponse
	if err := json.Unmarshal(rec.Body, &resp); err != nil {
		t.Fatalf("failed to unmarshal runtime response: %v", err)
	}
	if resp.Goroutines < 1 {
		t.Fatal("expected goroutines >= 1")
	}
	if resp.NumCPU < 1 {
		t.Fatal("expected num_cpu >= 1")
	}
	if resp.GOMAXPROCS < 1 {
		t.Fatal("expected gomaxprocs >= 1")
	}
	if resp.NumCPU != runtime.NumCPU() {
		t.Fatalf("num_cpu: got %d, want %d", resp.NumCPU, runtime.NumCPU())
	}
	if resp.GOMAXPROCS != runtime.GOMAXPROCS(0) {
		t.Fatalf("gomaxprocs: got %d, want %d", resp.GOMAXPROCS, runtime.GOMAXPROCS(0))
	}
}

func TestRuntimeEndpointDisabled(t *testing.T) {
	mw := New(Config{
		AuthFunc: openAuth(),
		Endpoints: map[string]bool{
			"status":  true,
			"runtime": false,
		},
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/runtime")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 404)
}

func TestRuntimeEndpointInIndex(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var endpoints []string
	if err := json.Unmarshal(rec.Body, &endpoints); err != nil {
		t.Fatalf("failed to unmarshal index response: %v", err)
	}
	found := false
	for _, ep := range endpoints {
		if ep == "/debug/celeris/runtime" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected /debug/celeris/runtime in index, got %v", endpoints)
	}
}

func TestMemStatsCacheConcurrent(t *testing.T) {
	mw := New(Config{
		AuthFunc:    openAuth(),
		MemStatsTTL: 5 * time.Second,
	})

	const goroutines = 10
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()
			rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/memory")
			if err != nil {
				errs <- err
				return
			}
			if rec.StatusCode != 200 {
				errs <- fmt.Errorf("expected status 200, got %d", rec.StatusCode)
				return
			}
			var resp memoryResponse
			if err := json.Unmarshal(rec.Body, &resp); err != nil {
				errs <- err
				return
			}
			if resp.Sys == 0 {
				errs <- errors.New("expected non-zero sys")
				return
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent memory endpoint error: %v", err)
	}
}

func TestIndexEndpointRequiresAuth(t *testing.T) {
	mw := New(Config{
		AuthFunc: func(_ *celeris.Context) bool { return false },
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 403)
}

func TestValidatePrefixMustStartWithSlash(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic for Prefix not starting with /")
		}
		msg, ok := r.(string)
		if !ok || msg != "debug: Prefix must start with /" {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	New(Config{Prefix: "no-slash"})
}

func TestNegativeMemStatsTTLUsesDefault(t *testing.T) {
	cfg := applyDefaults(Config{Prefix: "/debug", MemStatsTTL: -1 * time.Second})
	if cfg.MemStatsTTL != time.Second {
		t.Fatalf("negative MemStatsTTL should use default: got %v, want 1s", cfg.MemStatsTTL)
	}
}

func TestMemStatsTTLFloor(t *testing.T) {
	cfg := applyDefaults(Config{Prefix: "/debug", MemStatsTTL: time.Millisecond})
	if cfg.MemStatsTTL != 100*time.Millisecond {
		t.Fatalf("MemStatsTTL below floor should be clamped to 100ms: got %v", cfg.MemStatsTTL)
	}
	// Exactly at the floor should remain unchanged.
	cfg = applyDefaults(Config{Prefix: "/debug", MemStatsTTL: 100 * time.Millisecond})
	if cfg.MemStatsTTL != 100*time.Millisecond {
		t.Fatalf("MemStatsTTL at floor should stay 100ms: got %v", cfg.MemStatsTTL)
	}
	// Above the floor should remain unchanged.
	cfg = applyDefaults(Config{Prefix: "/debug", MemStatsTTL: 500 * time.Millisecond})
	if cfg.MemStatsTTL != 500*time.Millisecond {
		t.Fatalf("MemStatsTTL above floor should stay 500ms: got %v", cfg.MemStatsTTL)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "POST", "/debug/celeris/status")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 405)
}

func TestMethodNotAllowedIndex(t *testing.T) {
	mw := New(Config{AuthFunc: openAuth()})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "DELETE", "/debug/celeris")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 405)
}

func TestDefaultConfigImmutable(t *testing.T) {
	cfg1 := defaultConfigCopy()
	cfg1.Prefix = "/changed"
	cfg2 := defaultConfigCopy()
	if cfg2.Prefix != "/debug/celeris" {
		t.Fatalf("defaultConfigCopy() mutation leaked: got %q, want /debug/celeris", cfg2.Prefix)
	}
}

func TestRemoteAddrWithoutPort(t *testing.T) {
	mw := New()
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/debug/celeris/status",
		celeristest.WithRemoteAddr("127.0.0.1"))
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}
