package celeris

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
)

// TestRegistryCardinality verifies the 35-config expectation on Linux and
// the Std-only fallback (5) on every other host. No duplicate names may
// appear regardless of platform.
func TestRegistryCardinality(t *testing.T) {
	reg := servers.Registry()

	var celerisServers []servers.Server
	for _, s := range reg {
		if s.Kind() == "celeris" {
			celerisServers = append(celerisServers, s)
		}
	}

	switch runtime.GOOS {
	case "linux":
		if got, want := len(celerisServers), 35; got != want {
			names := make([]string, 0, got)
			for _, s := range celerisServers {
				names = append(names, s.Name())
			}
			t.Fatalf("Registry returned %d celeris servers, want %d\nnames:\n%s",
				got, want, strings.Join(names, "\n"))
		}
	default:
		// On non-Linux we register only the 5 Std configs.
		if got, lowerBound := len(celerisServers), 5; got < lowerBound {
			t.Fatalf("Registry returned %d celeris servers on %s, want >= %d",
				got, runtime.GOOS, lowerBound)
		}
		if got, want := len(celerisServers), 5; got != want {
			t.Fatalf("Registry returned %d celeris servers on %s, want exactly %d (std-only)",
				got, runtime.GOOS, want)
		}
	}

	seen := make(map[string]struct{}, len(celerisServers))
	for _, s := range celerisServers {
		n := s.Name()
		if _, dup := seen[n]; dup {
			t.Errorf("duplicate celeris server name: %s", n)
		}
		seen[n] = struct{}{}
	}
}

// TestStartStopGoroutineStable is the regression guard for the
// thread-exhaustion crash at cell 1021 in the 2026-04-23 matrix run.
// The root cause was celeris.Server.StartWithListener using a
// context.Background() the perfmatrix Stop could never cancel; the
// native engines' worker goroutines (12 per cell) therefore never
// exited, and Go hit its 10000-thread limit.
//
// On Linux we cycle an iouring-h1 cell 30 times and assert the
// goroutine delta is tiny; on other platforms we cycle a std-h1 cell
// (still enough to catch any general Start/Stop leak).
func TestStartStopGoroutineStable(t *testing.T) {
	name := "celeris-iouring-h1-sync"
	cycles := 30
	if runtime.GOOS != "linux" {
		name = "celeris-std-h1"
		cycles = 10
	}

	var target servers.Server
	for _, s := range servers.Registry() {
		if s.Name() == name {
			target = s
			break
		}
	}
	if target == nil {
		t.Skipf("%s not in registry on %s", name, runtime.GOOS)
	}

	// Warm up once so any one-off singletons (pools, caches) land
	// before the baseline snapshot.
	ln, err := target.Start(context.Background(), nil)
	if err != nil {
		t.Skipf("warm-up Start failed (engine probably unsupported): %v", err)
	}
	_ = ln
	if err := target.Stop(context.Background()); err != nil {
		t.Fatalf("warm-up Stop: %v", err)
	}

	// Let any trailing goroutines from warm-up exit before snapshotting.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	baseline := runtime.NumGoroutine()

	for i := 0; i < cycles; i++ {
		ln, err := target.Start(context.Background(), nil)
		if err != nil {
			t.Fatalf("cycle %d: Start: %v", i, err)
		}
		_ = ln
		if err := target.Stop(context.Background()); err != nil {
			t.Fatalf("cycle %d: Stop: %v", i, err)
		}
	}

	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	// If every cycle leaked even a single goroutine, after-baseline
	// would be ≥ cycles. Allow a small constant slack for GC workers
	// and other stdlib background goroutines that may idle-start.
	const slack = 10
	if after-baseline > slack {
		t.Fatalf("goroutine leak: baseline=%d after %d cycles=%d (leak=%d, slack=%d)",
			baseline, cycles, after, after-baseline, slack)
	}
}

// TestAllNamesLowercase enforces the "lowercase throughout" rule from the
// naming convention docstring.
func TestAllNamesLowercase(t *testing.T) {
	for _, s := range servers.Registry() {
		if s.Kind() != "celeris" {
			continue
		}
		if got := s.Name(); got != strings.ToLower(got) {
			t.Errorf("server name not lowercase: %q", got)
		}
	}
}

// TestStdH1Smoke boots the celeris-std-h1 cell-column, exercises every
// static handler, then shuts it down cleanly. Smoke coverage guards the
// Start/Stop plumbing on every platform (Std is always available).
func TestStdH1Smoke(t *testing.T) {
	var target servers.Server
	for _, s := range servers.Registry() {
		if s.Name() == "celeris-std-h1" {
			target = s
			break
		}
	}
	if target == nil {
		t.Fatal("celeris-std-h1 not in registry")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ln, err := target.Start(ctx, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := target.Stop(shutdownCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	base := "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}

	// GET / → "Hello, World!" (13 bytes).
	resp, err := client.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "Hello, World!" {
		t.Fatalf("GET / = %d %q, want 200 \"Hello, World!\"", resp.StatusCode, body)
	}

	// GET /json → small JSON object.
	resp, err = client.Get(base + "/json")
	if err != nil {
		t.Fatalf("GET /json: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /json status = %d, body %q", resp.StatusCode, body)
	}
	var small payloadSmall
	if err := json.Unmarshal(body, &small); err != nil {
		t.Fatalf("GET /json decode: %v (body=%q)", err, body)
	}
	if small.Server != "celeris" {
		t.Fatalf("GET /json server field = %q, want \"celeris\"", small.Server)
	}

	// GET /json-1k → ~1 KiB JSON.
	resp, err = client.Get(base + "/json-1k")
	if err != nil {
		t.Fatalf("GET /json-1k: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(body) < 512 {
		t.Fatalf("GET /json-1k status=%d body-len=%d (want >=512)", resp.StatusCode, len(body))
	}

	// GET /json-64k → ~64 KiB JSON.
	resp, err = client.Get(base + "/json-64k")
	if err != nil {
		t.Fatalf("GET /json-64k: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(body) < 32*1024 {
		t.Fatalf("GET /json-64k status=%d body-len=%d (want >=32768)", resp.StatusCode, len(body))
	}

	// GET /users/:id → echoes id.
	resp, err = client.Get(base + "/users/42")
	if err != nil {
		t.Fatalf("GET /users/42: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "User ID: 42" {
		t.Fatalf("GET /users/42 = %d %q", resp.StatusCode, body)
	}

	// POST /upload → "OK".
	resp, err = client.Post(base+"/upload", "application/octet-stream",
		strings.NewReader("hello-upload-body"))
	if err != nil {
		t.Fatalf("POST /upload: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "OK" {
		t.Fatalf("POST /upload = %d %q", resp.StatusCode, body)
	}
}

// TestFeatureFlagsSanity walks every registered celeris server and
// verifies that the FeatureSet matches what the cell-column name
// implies. This guards against drift between the name builder and the
// feature builder in newCelerisServer.
func TestFeatureFlagsSanity(t *testing.T) {
	for _, s := range servers.Registry() {
		if s.Kind() != "celeris" {
			continue
		}
		f := s.Features()
		name := s.Name()

		if !f.HTTP1 {
			t.Errorf("%s: HTTP1=false, want true for all celeris configs", name)
		}
		if !f.Drivers {
			t.Errorf("%s: Drivers=false, want true", name)
		}
		if !f.Middleware {
			t.Errorf("%s: Middleware=false, want true", name)
		}

		protocol := parseProtocolFromName(t, name)
		switch protocol {
		case "h1":
			if f.HTTP2C {
				t.Errorf("%s: HTTP2C=true for H1-only config", name)
			}
			if f.Auto {
				t.Errorf("%s: Auto=true for H1-only config", name)
			}
		case "h2c":
			if !f.HTTP2C {
				t.Errorf("%s: HTTP2C=false for H2C config", name)
			}
		case "auto":
			if !f.Auto {
				t.Errorf("%s: Auto=false for Auto config", name)
			}
			if !f.HTTP2C {
				t.Errorf("%s: HTTP2C=false for Auto config (auto implies H2C support)", name)
			}
		}

		wantUpgrade := strings.Contains(name, "+upg")
		if f.H2CUpgrade != wantUpgrade {
			t.Errorf("%s: H2CUpgrade=%v, want %v", name, f.H2CUpgrade, wantUpgrade)
		}

		wantAsync := strings.HasSuffix(name, "-async")
		if f.AsyncHandlers != wantAsync {
			t.Errorf("%s: AsyncHandlers=%v, want %v", name, f.AsyncHandlers, wantAsync)
		}
	}
}

// parseProtocolFromName extracts the protocol segment ("h1"/"h2c"/"auto")
// from a celeris cell-column name like "celeris-epoll-h2c+upg-async".
func parseProtocolFromName(t *testing.T, name string) string {
	t.Helper()
	// celeris-<engine>-<protocol>[...]
	parts := strings.SplitN(name, "-", 3)
	if len(parts) < 3 {
		t.Fatalf("unexpected name shape: %q", name)
	}
	tail := parts[2]
	for _, p := range []string{"h1", "h2c", "auto"} {
		if tail == p || strings.HasPrefix(tail, p+"+") || strings.HasPrefix(tail, p+"-") {
			return p
		}
	}
	t.Fatalf("cannot parse protocol from %q (tail=%q)", name, tail)
	return ""
}

// Ensure the server list is non-empty; a silent init() regression would
// otherwise turn every other test in this package into a tautology.
func TestRegistryNonEmpty(t *testing.T) {
	reg := servers.Registry()
	count := 0
	for _, s := range reg {
		if s.Kind() == "celeris" {
			count++
		}
	}
	if count == 0 {
		t.Fatal("no celeris servers registered — init() broken")
	}
	fmt.Printf("celeris perfmatrix: %d servers registered on %s\n", count, runtime.GOOS)
}
