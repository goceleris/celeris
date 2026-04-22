package scenarios

import (
	"sort"
	"testing"

	"github.com/goceleris/celeris/test/perfmatrix/servers"
)

// expectedRegistry lists every scenario registered by wave-2F's static.go
// and concurrency.go init()s. Chain and driver scenarios are wired in
// other files and are deliberately excluded here — this test guards the
// slice we own, not the ones we don't.
var expectedRegistry = []string{
	// static (8)
	"churn-close",
	"get-json",
	"get-json-1k",
	"get-json-64k",
	"get-simple",
	"post-1m",
	"post-4k",
	"post-64k",

	// concurrency (4)
	"auto-mix-111",
	"get-json-1c",
	"get-simple-1024c",
	"get-simple-128c",
}

func TestRegistryContainsWave2FScenarios(t *testing.T) {
	t.Parallel()
	names := make(map[string]bool)
	for _, s := range Registry() {
		names[s.Name()] = true
	}
	for _, want := range expectedRegistry {
		if !names[want] {
			t.Errorf("scenario %q missing from Registry()", want)
		}
	}

	// The registry may legitimately hold more entries (chain/driver
	// scenarios landed by sibling agents); filter to the static+concurrency
	// categories before demanding an exact count.
	var got []string
	for _, s := range Registry() {
		switch s.Category() {
		case CategoryStatic, CategoryConcurrency:
			got = append(got, s.Name())
		}
	}
	sort.Strings(got)

	want := append([]string(nil), expectedRegistry...)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("static+concurrency registry size = %d, want %d\n got=%v\nwant=%v",
			len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("static+concurrency registry[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWorkloadConfigIsSane(t *testing.T) {
	t.Parallel()
	const target = "http://127.0.0.1:8080"
	for _, name := range expectedRegistry {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := findScenario(t, name)
			cfg := s.Workload(target)
			if cfg.URL == "" {
				t.Errorf("Workload(%q).URL is empty", target)
			}
			if cfg.Connections <= 0 {
				t.Errorf("Workload(%q).Connections = %d, want > 0", target, cfg.Connections)
			}
			// Duration + Warmup are filled in by the orchestrator, not here.
			// A non-zero value would mean a scenario accidentally hard-codes
			// a bench time and overrides the CLI flag.
			if cfg.Duration != 0 {
				t.Errorf("Workload(%q).Duration = %s, want 0 (runner fills it)", target, cfg.Duration)
			}
			if cfg.Warmup != 0 {
				t.Errorf("Workload(%q).Warmup = %s, want 0 (runner fills it)", target, cfg.Warmup)
			}
		})
	}
}

func TestStaticPOSTBodiesExactSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want int
	}{
		{"post-4k", 4 * 1024},
		{"post-64k", 64 * 1024},
		{"post-1m", 1024 * 1024},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := findScenario(t, tc.name)
			stat, ok := s.(*StaticScenario)
			if !ok {
				t.Fatalf("scenario %q is %T, want *StaticScenario", tc.name, s)
			}
			if got := len(stat.Body); got != tc.want {
				t.Errorf("POST body size for %q = %d, want %d", tc.name, got, tc.want)
			}
			cfg := stat.Workload("http://x")
			if got := len(cfg.Body); got != tc.want {
				t.Errorf("Workload.Body size for %q = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestStaticScenariosAlwaysApplicable(t *testing.T) {
	t.Parallel()
	empty := servers.FeatureSet{}
	for _, name := range []string{
		"churn-close", "get-json", "get-json-1k", "get-json-64k",
		"get-simple", "post-1m", "post-4k", "post-64k",
	} {
		s := findScenario(t, name)
		if !s.Applicable(empty) {
			t.Errorf("static scenario %q unexpectedly skipped for empty FeatureSet", name)
		}
	}
}

func TestChurnCloseUsesConnectionClose(t *testing.T) {
	t.Parallel()
	s := findScenario(t, "churn-close")
	cfg := s.Workload("http://x")
	if !cfg.DisableKeepAlive {
		t.Errorf("churn-close: DisableKeepAlive = false, want true")
	}
	if cfg.Connections != 32 {
		t.Errorf("churn-close: Connections = %d, want 32", cfg.Connections)
	}
}

func TestAutoMixApplicableGating(t *testing.T) {
	t.Parallel()
	s := findScenario(t, "auto-mix-111")

	// Missing any of H1 / H2C / H2CUpgrade → inapplicable.
	checks := []struct {
		name string
		fs   servers.FeatureSet
		want bool
	}{
		{"empty", servers.FeatureSet{}, false},
		{"only-h1", servers.FeatureSet{HTTP1: true}, false},
		{"h1+h2c", servers.FeatureSet{HTTP1: true, HTTP2C: true}, false},
		{"h1+h2c+upgrade", servers.FeatureSet{HTTP1: true, HTTP2C: true, H2CUpgrade: true}, true},
		{"everything", servers.FeatureSet{
			HTTP1: true, HTTP2C: true, Auto: true, H2CUpgrade: true,
			Drivers: true, Middleware: true, AsyncHandlers: true,
		}, true},
	}
	for _, c := range checks {
		if got := s.Applicable(c.fs); got != c.want {
			t.Errorf("auto-mix-111 Applicable(%s) = %v, want %v", c.name, got, c.want)
		}
	}

	// The Mix config must carry the triple 1:1:1 and must NOT set HTTP2
	// or H2CUpgrade (mutually exclusive with Mix per loadgen godoc).
	cfg := s.Workload("http://x")
	if cfg.Mix == nil {
		t.Fatalf("auto-mix-111: Workload.Mix == nil, want *loadgen.MixRatio{1,1,1}")
	}
	if cfg.Mix.H1 != 1 || cfg.Mix.H2 != 1 || cfg.Mix.Upgrade != 1 {
		t.Errorf("auto-mix-111: Mix = %+v, want {1,1,1}", *cfg.Mix)
	}
	if cfg.HTTP2 {
		t.Errorf("auto-mix-111: HTTP2 = true, must be false when Mix is set")
	}
	if cfg.H2CUpgrade {
		t.Errorf("auto-mix-111: H2CUpgrade = true, must be false when Mix is set")
	}
}

func TestConcurrencyNonAutoMixAlwaysApplicable(t *testing.T) {
	t.Parallel()
	empty := servers.FeatureSet{}
	for _, name := range []string{"get-json-1c", "get-simple-128c", "get-simple-1024c"} {
		s := findScenario(t, name)
		if !s.Applicable(empty) {
			t.Errorf("%q: unexpectedly skipped for empty FeatureSet", name)
		}
	}
}

func TestCategories(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"churn-close", "get-json", "get-json-1k", "get-json-64k",
		"get-simple", "post-1m", "post-4k", "post-64k",
	} {
		s := findScenario(t, name)
		if got := s.Category(); got != CategoryStatic {
			t.Errorf("%q: Category() = %q, want %q", name, got, CategoryStatic)
		}
	}
	for _, name := range []string{
		"auto-mix-111", "get-json-1c", "get-simple-128c", "get-simple-1024c",
	} {
		s := findScenario(t, name)
		if got := s.Category(); got != CategoryConcurrency {
			t.Errorf("%q: Category() = %q, want %q", name, got, CategoryConcurrency)
		}
	}
}

// findScenario locates a registered scenario by name; fails the test if
// missing so downstream assertions can assume non-nil.
func findScenario(t *testing.T, name string) Scenario {
	t.Helper()
	for _, s := range Registry() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("scenario %q not registered", name)
	return nil
}
