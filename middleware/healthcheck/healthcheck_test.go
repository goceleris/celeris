package healthcheck

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/celeristest"

	"github.com/goceleris/celeris/middleware/internal/testutil"
)

type probeResponse struct {
	Status string `json:"status"`
}

func okHandler(c *celeris.Context) error {
	return c.String(200, "ok")
}

func TestDefaultProbes(t *testing.T) {
	t.Parallel()
	mw := New()
	for _, path := range []string{"/livez", "/readyz", "/startupz"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", path)
			testutil.AssertNoError(t, err)
			testutil.AssertStatus(t, rec, 200)

			var resp probeResponse
			if err := json.Unmarshal(rec.Body, &resp); err != nil {
				t.Fatalf("failed to unmarshal response: %v", err)
			}
			if resp.Status != "ok" {
				t.Fatalf("status: got %q, want %q", resp.Status, "ok")
			}
		})
	}
}

func TestCheckerReturnsFalse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  Config
		path string
	}{
		{"live", Config{LivePath: "/livez", ReadyPath: "/readyz", StartPath: "/startupz", LiveChecker: func(_ *celeris.Context) bool { return false }}, "/livez"},
		{"ready", Config{LivePath: "/livez", ReadyPath: "/readyz", StartPath: "/startupz", ReadyChecker: func(_ *celeris.Context) bool { return false }}, "/readyz"},
		{"start", Config{LivePath: "/livez", ReadyPath: "/readyz", StartPath: "/startupz", StartChecker: func(_ *celeris.Context) bool { return false }}, "/startupz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw := New(tt.cfg)
			rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", tt.path)
			testutil.AssertNoError(t, err)
			testutil.AssertStatus(t, rec, 503)

			var resp probeResponse
			if err := json.Unmarshal(rec.Body, &resp); err != nil {
				t.Fatalf("failed to unmarshal response: %v", err)
			}
			if resp.Status != "unavailable" {
				t.Fatalf("status: got %q, want %q", resp.Status, "unavailable")
			}
		})
	}
}

func TestNonProbePath(t *testing.T) {
	t.Parallel()
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/api/users")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestCustomPaths(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		LivePath:  "/health/live",
		ReadyPath: "/health/ready",
		StartPath: "/health/startup",
	})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/health/live")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/health/ready")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/health/startup")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	// Original default paths should pass through.
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err = testutil.RunChain(t, chain, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestDefaultConfigPaths(t *testing.T) {
	t.Parallel()
	cfg := defaultConfigCopy()
	if cfg.LivePath != "/livez" {
		t.Fatalf("defaultConfigCopy().LivePath: got %q, want %q", cfg.LivePath, "/livez")
	}
	if cfg.ReadyPath != "/readyz" {
		t.Fatalf("defaultConfigCopy().ReadyPath: got %q, want %q", cfg.ReadyPath, "/readyz")
	}
	if cfg.StartPath != "/startupz" {
		t.Fatalf("defaultConfigCopy().StartPath: got %q, want %q", cfg.StartPath, "/startupz")
	}
}

func TestDefaultConfigCheckers(t *testing.T) {
	t.Parallel()
	cfg := defaultConfigCopy()
	if cfg.LiveChecker == nil {
		t.Fatal("defaultConfigCopy().LiveChecker should not be nil")
	}
	if cfg.ReadyChecker == nil {
		t.Fatal("defaultConfigCopy().ReadyChecker should not be nil")
	}
	if cfg.StartChecker == nil {
		t.Fatal("defaultConfigCopy().StartChecker should not be nil")
	}
	if !cfg.LiveChecker(nil) {
		t.Fatal("defaultConfigCopy().LiveChecker should return true")
	}
	if !cfg.ReadyChecker(nil) {
		t.Fatal("defaultConfigCopy().ReadyChecker should return true")
	}
	if !cfg.StartChecker(nil) {
		t.Fatal("defaultConfigCopy().StartChecker should return true")
	}
}

func TestApplyDefaultsLeavesEmptyPaths(t *testing.T) {
	t.Parallel()
	cfg := applyDefaults(Config{})
	if cfg.LivePath != "" {
		t.Fatalf("applyDefaults LivePath: got %q, want empty", cfg.LivePath)
	}
	if cfg.ReadyPath != "" {
		t.Fatalf("applyDefaults ReadyPath: got %q, want empty", cfg.ReadyPath)
	}
	if cfg.StartPath != "" {
		t.Fatalf("applyDefaults StartPath: got %q, want empty", cfg.StartPath)
	}
}

func TestApplyDefaultsFillsCheckers(t *testing.T) {
	t.Parallel()
	cfg := applyDefaults(Config{})
	if cfg.LiveChecker == nil || cfg.ReadyChecker == nil || cfg.StartChecker == nil {
		t.Fatal("applyDefaults should fill nil checkers")
	}
}

func TestApplyDefaultsPreservesCustom(t *testing.T) {
	t.Parallel()
	custom := func(_ *celeris.Context) bool { return false }
	cfg := applyDefaults(Config{
		LivePath:     "/l",
		ReadyPath:    "/r",
		StartPath:    "/s",
		LiveChecker:  custom,
		ReadyChecker: custom,
		StartChecker: custom,
	})
	if cfg.LivePath != "/l" {
		t.Fatalf("applyDefaults LivePath: got %q, want %q", cfg.LivePath, "/l")
	}
	if cfg.ReadyPath != "/r" {
		t.Fatalf("applyDefaults ReadyPath: got %q, want %q", cfg.ReadyPath, "/r")
	}
	if cfg.StartPath != "/s" {
		t.Fatalf("applyDefaults StartPath: got %q, want %q", cfg.StartPath, "/s")
	}
	if cfg.LiveChecker(nil) {
		t.Fatal("custom LiveChecker should return false")
	}
}

func TestPOSTMethodPassesThrough(t *testing.T) {
	t.Parallel()
	mw := New()
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "POST", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestHEADMethodAllowed(t *testing.T) {
	t.Parallel()
	mw := New()
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "HEAD", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyEmpty(t, rec)
}

func TestExportedPathConstants(t *testing.T) {
	t.Parallel()
	if DefaultLivePath != "/livez" {
		t.Fatalf("DefaultLivePath: got %q, want %q", DefaultLivePath, "/livez")
	}
	if DefaultReadyPath != "/readyz" {
		t.Fatalf("DefaultReadyPath: got %q, want %q", DefaultReadyPath, "/readyz")
	}
	if DefaultStartPath != "/startupz" {
		t.Fatalf("DefaultStartPath: got %q, want %q", DefaultStartPath, "/startupz")
	}
}

func TestFastPathTimeoutConstant(t *testing.T) {
	t.Parallel()
	if FastPathTimeout != -1*time.Second {
		t.Fatalf("FastPathTimeout: got %v, want -1s", FastPathTimeout)
	}
}

func FuzzHealthcheckPaths(f *testing.F) {
	f.Add("/livez")
	f.Add("/readyz")
	f.Add("/startupz")
	f.Add("/api/users")
	f.Add("")
	f.Add("/")
	f.Add("/livez/extra")
	f.Fuzz(func(t *testing.T, path string) {
		mw := New()
		ctx, _ := celeristest.NewContextT(t, "GET", path)
		_ = mw(ctx)
	})
}

func TestContentTypeJSON(t *testing.T) {
	t.Parallel()
	mw := New()
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertHeaderContains(t, rec, "content-type", "application/json")
}

func TestCheckerReceivesContext(t *testing.T) {
	t.Parallel()
	var received bool
	mw := New(Config{
		LivePath:  "/livez",
		ReadyPath: "/readyz",
		StartPath: "/startupz",
		LiveChecker: func(c *celeris.Context) bool {
			received = c != nil
			return true
		},
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if !received {
		t.Fatal("expected checker to receive non-nil context")
	}
}

func TestSkipBypassesHealthcheck(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		LivePath:  "/livez",
		ReadyPath: "/readyz",
		StartPath: "/startupz",
		Skip:      func(_ *celeris.Context) bool { return true },
	})
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestSkipFalseStillMatchesProbe(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		LivePath:  "/livez",
		ReadyPath: "/readyz",
		StartPath: "/startupz",
		Skip:      func(_ *celeris.Context) bool { return false },
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var resp probeResponse
	if err := json.Unmarshal(rec.Body, &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status: got %q, want %q", resp.Status, "ok")
	}
}

func TestCheckerTimeoutDefault(t *testing.T) {
	t.Parallel()
	cfg := applyDefaults(Config{})
	if cfg.CheckerTimeout != 5*time.Second {
		t.Fatalf("default CheckerTimeout: got %v, want 5s", cfg.CheckerTimeout)
	}
}

func TestCheckerTimeoutExceeded(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		LivePath:       "/livez",
		ReadyPath:      "/readyz",
		StartPath:      "/startupz",
		CheckerTimeout: 10 * time.Millisecond,
		LiveChecker: func(_ *celeris.Context) bool {
			time.Sleep(100 * time.Millisecond)
			return true
		},
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 503)

	var resp probeResponse
	if err := json.Unmarshal(rec.Body, &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Status != "unavailable" {
		t.Fatalf("status: got %q, want %q", resp.Status, "unavailable")
	}
}

func TestCheckerCompletesWithinTimeout(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		LivePath:       "/livez",
		ReadyPath:      "/readyz",
		StartPath:      "/startupz",
		CheckerTimeout: 1 * time.Second,
		LiveChecker: func(_ *celeris.Context) bool {
			return true
		},
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestCheckerTimeoutAppliedToReady(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		LivePath:       "/livez",
		ReadyPath:      "/readyz",
		StartPath:      "/startupz",
		CheckerTimeout: 10 * time.Millisecond,
		ReadyChecker: func(_ *celeris.Context) bool {
			time.Sleep(100 * time.Millisecond)
			return true
		},
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/readyz")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 503)
}

func TestCheckerTimeoutCustomValue(t *testing.T) {
	t.Parallel()
	cfg := applyDefaults(Config{CheckerTimeout: 10 * time.Second})
	if cfg.CheckerTimeout != 10*time.Second {
		t.Fatalf("custom CheckerTimeout: got %v, want 10s", cfg.CheckerTimeout)
	}
}

func TestCheckerContextCancellation(t *testing.T) {
	t.Parallel()
	ctxDone := make(chan struct{})
	mw := New(Config{
		LivePath:       "/livez",
		ReadyPath:      "/readyz",
		StartPath:      "/startupz",
		CheckerTimeout: 50 * time.Millisecond,
		LiveChecker: func(c *celeris.Context) bool {
			// Capture the context immediately to avoid racing with
			// context reset after the middleware returns.
			ctx := c.Context()
			<-ctx.Done()
			close(ctxDone)
			return false
		},
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 503)

	select {
	case <-ctxDone:
		// Checker observed context cancellation.
	case <-time.After(2 * time.Second):
		t.Fatal("expected checker to observe context cancellation")
	}
}

// --- Fast-path (no timeout) tests ---

func TestFastPathNoTimeout(t *testing.T) {
	t.Parallel()
	called := false
	mw := New(Config{
		LivePath:       "/livez",
		ReadyPath:      "/readyz",
		StartPath:      "/startupz",
		CheckerTimeout: FastPathTimeout, // negative = fast-path, no goroutine
		LiveChecker: func(_ *celeris.Context) bool {
			called = true
			return true
		},
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	if !called {
		t.Fatal("expected checker to be called synchronously")
	}

	var resp probeResponse
	if err := json.Unmarshal(rec.Body, &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status: got %q, want %q", resp.Status, "ok")
	}
}

func TestFastPathReturnsFalse(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		LivePath:       "/livez",
		ReadyPath:      "/readyz",
		StartPath:      "/startupz",
		CheckerTimeout: FastPathTimeout,
		ReadyChecker: func(_ *celeris.Context) bool {
			return false
		},
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/readyz")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 503)

	var resp probeResponse
	if err := json.Unmarshal(rec.Body, &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Status != "unavailable" {
		t.Fatalf("status: got %q, want %q", resp.Status, "unavailable")
	}
}

func TestFastPathAllProbes(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		LivePath:       "/livez",
		ReadyPath:      "/readyz",
		StartPath:      "/startupz",
		CheckerTimeout: FastPathTimeout,
		LiveChecker:    func(_ *celeris.Context) bool { return true },
		ReadyChecker:   func(_ *celeris.Context) bool { return false },
		StartChecker:   func(_ *celeris.Context) bool { return true },
	})

	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/readyz")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 503)

	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/startupz")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

// --- Validation panic tests ---

func TestValidateAcceptsEmptyPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  Config
	}{
		{"empty LivePath", Config{LivePath: "", ReadyPath: "/r", StartPath: "/s"}},
		{"empty ReadyPath", Config{LivePath: "/l", ReadyPath: "", StartPath: "/s"}},
		{"empty StartPath", Config{LivePath: "/l", ReadyPath: "/r", StartPath: ""}},
		{"all empty", Config{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.cfg.validate() // should not panic
		})
	}
}

func TestValidatePanicsOnMissingSlash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  Config
	}{
		{"LivePath no slash", Config{LivePath: "livez", ReadyPath: "/r", StartPath: "/s"}},
		{"ReadyPath no slash", Config{LivePath: "/l", ReadyPath: "readyz", StartPath: "/s"}},
		{"StartPath no slash", Config{LivePath: "/l", ReadyPath: "/r", StartPath: "startupz"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic for path without leading /")
				}
			}()
			tt.cfg.validate()
		})
	}
}

func TestValidatePanicsOnOverlappingPaths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  Config
	}{
		{"live==ready", Config{LivePath: "/health", ReadyPath: "/health", StartPath: "/s"}},
		{"live==start", Config{LivePath: "/health", ReadyPath: "/r", StartPath: "/health"}},
		{"ready==start", Config{LivePath: "/l", ReadyPath: "/health", StartPath: "/health"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic for overlapping paths")
				}
			}()
			tt.cfg.validate()
		})
	}
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	t.Parallel()
	// Should not panic.
	cfg := Config{
		LivePath:  "/l",
		ReadyPath: "/r",
		StartPath: "/s",
	}
	cfg.validate()
}

// --- HEAD body suppression tests ---

func TestHEADNoBody(t *testing.T) {
	t.Parallel()
	mw := New()
	for _, path := range []string{"/livez", "/readyz", "/startupz"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			rec, err := testutil.RunMiddlewareWithMethod(t, mw, "HEAD", path)
			testutil.AssertNoError(t, err)
			testutil.AssertStatus(t, rec, 200)
			testutil.AssertBodyEmpty(t, rec)
		})
	}
}

func TestHEADUnavailableNoBody(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		LivePath:    "/livez",
		ReadyPath:   "/readyz",
		StartPath:   "/startupz",
		LiveChecker: func(_ *celeris.Context) bool { return false },
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "HEAD", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 503)
	testutil.AssertBodyEmpty(t, rec)
}

func TestGETStillReturnsBody(t *testing.T) {
	t.Parallel()
	mw := New()
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	var resp probeResponse
	if err := json.Unmarshal(rec.Body, &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Status != "ok" {
		t.Fatalf("status: got %q, want %q", resp.Status, "ok")
	}
}

// --- Disabled probe tests ---

func TestDisabledStartupProbePassesThrough(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		LivePath:  "/livez",
		ReadyPath: "/readyz",
		StartPath: "", // disabled
	})

	// Enabled probes still work.
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/readyz")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)

	// Disabled probe path passes through to next handler.
	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err = testutil.RunChain(t, chain, "GET", "/startupz")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")
}

func TestDisabledLiveProbePassesThrough(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		LivePath:  "", // disabled
		ReadyPath: "/readyz",
		StartPath: "/startupz",
	})

	chain := []celeris.HandlerFunc{mw, okHandler}
	rec, err := testutil.RunChain(t, chain, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
	testutil.AssertBodyContains(t, rec, "ok")

	// Enabled probes still respond.
	rec, err = testutil.RunMiddlewareWithMethod(t, mw, "GET", "/readyz")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 200)
}

func TestDisabledAllProbesPassesThrough(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		LivePath:  "",
		ReadyPath: "",
		StartPath: "",
	})

	chain := []celeris.HandlerFunc{mw, okHandler}
	for _, path := range []string{"/livez", "/readyz", "/startupz", "/other"} {
		rec, err := testutil.RunChain(t, chain, "GET", path)
		testutil.AssertNoError(t, err)
		testutil.AssertStatus(t, rec, 200)
		testutil.AssertBodyContains(t, rec, "ok")
	}
}

func TestValidateAcceptsDisabledWithEnabled(t *testing.T) {
	t.Parallel()
	// Two enabled probes with same path should still panic.
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for overlapping enabled paths")
		}
	}()
	cfg := Config{LivePath: "/health", ReadyPath: "/health", StartPath: ""}
	cfg.validate()
}

// --- Checker panic recovery tests ---

func TestCheckerPanicReturns503(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		LivePath:       "/livez",
		ReadyPath:      "/readyz",
		StartPath:      "/startupz",
		CheckerTimeout: 1 * time.Second,
		LiveChecker: func(_ *celeris.Context) bool {
			panic("boom")
		},
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 503)
}

func TestCheckerPanicFastPathReturns503(t *testing.T) {
	t.Parallel()
	mw := New(Config{
		LivePath:       "/livez",
		ReadyPath:      "/readyz",
		StartPath:      "/startupz",
		CheckerTimeout: FastPathTimeout,
		LiveChecker: func(_ *celeris.Context) bool {
			panic("boom in fast-path")
		},
	})
	rec, err := testutil.RunMiddlewareWithMethod(t, mw, "GET", "/livez")
	testutil.AssertNoError(t, err)
	testutil.AssertStatus(t, rec, 503)
}

// --- DefaultConfig immutability ---

func TestDefaultConfigImmutable(t *testing.T) {
	t.Parallel()
	cfg1 := defaultConfigCopy()
	cfg1.LivePath = "/changed"
	cfg2 := defaultConfigCopy()
	if cfg2.LivePath != "/livez" {
		t.Fatalf("defaultConfigCopy() mutation leaked: got %q, want /livez", cfg2.LivePath)
	}
}

// --- Concurrent access test ---

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()
	mw := New()

	var wg sync.WaitGroup
	const goroutines = 10
	const iterations = 100

	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range iterations {
				ctx, _ := celeristest.NewContext("GET", "/livez")
				err := mw(ctx)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
				celeristest.ReleaseContext(ctx)
			}
		}()
	}
	wg.Wait()
}
