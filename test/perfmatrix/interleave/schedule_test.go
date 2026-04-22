package interleave

import (
	"context"
	"net"
	"testing"

	"github.com/goceleris/loadgen"

	"github.com/goceleris/celeris/test/perfmatrix/scenarios"
	"github.com/goceleris/celeris/test/perfmatrix/servers"
	"github.com/goceleris/celeris/test/perfmatrix/services"
)

// fakeScenario is a test-only scenarios.Scenario. applicable decides
// whether it matches a given FeatureSet — if nil, it's always applicable.
type fakeScenario struct {
	name       string
	category   string
	applicable func(servers.FeatureSet) bool
}

func (s *fakeScenario) Name() string                   { return s.name }
func (s *fakeScenario) Category() string               { return s.category }
func (s *fakeScenario) Workload(string) loadgen.Config { return loadgen.Config{} }
func (s *fakeScenario) Applicable(fs servers.FeatureSet) bool {
	if s.applicable == nil {
		return true
	}
	return s.applicable(fs)
}

// fakeServer is a test-only servers.Server.
type fakeServer struct {
	name     string
	kind     string
	features servers.FeatureSet
}

func (s *fakeServer) Name() string                 { return s.name }
func (s *fakeServer) Kind() string                 { return s.kind }
func (s *fakeServer) Features() servers.FeatureSet { return s.features }
func (s *fakeServer) Start(context.Context, *services.Handles) (net.Listener, error) {
	return nil, nil
}
func (s *fakeServer) Stop(context.Context) error { return nil }

func TestScheduleEmptyInputs(t *testing.T) {
	if got := Schedule(0, nil, nil); got != nil {
		t.Fatalf("Schedule(0, nil, nil) = %v, want nil", got)
	}
	if got := Schedule(3, nil, []servers.Server{&fakeServer{name: "s"}}); got != nil {
		t.Fatalf("Schedule with no scenarios should be nil, got %v", got)
	}
	if got := Schedule(3, []scenarios.Scenario{&fakeScenario{name: "x"}}, nil); got != nil {
		t.Fatalf("Schedule with no servers should be nil, got %v", got)
	}
}

func TestScheduleCoverageAndOrdering(t *testing.T) {
	ss := []scenarios.Scenario{
		&fakeScenario{name: "A"},
		&fakeScenario{name: "B"},
	}
	sv := []servers.Server{
		&fakeServer{name: "s1", features: servers.FeatureSet{HTTP1: true}},
		&fakeServer{name: "s2", features: servers.FeatureSet{HTTP1: true}},
		&fakeServer{name: "s3", features: servers.FeatureSet{HTTP1: true}},
	}
	runs := 4
	cells := Schedule(runs, ss, sv)

	wantLen := runs * len(ss) * len(sv)
	if len(cells) != wantLen {
		t.Fatalf("len(cells) = %d, want %d", len(cells), wantLen)
	}

	// Every (scenario, server) pair appears exactly `runs` times.
	counts := make(map[[2]string]int)
	for _, c := range cells {
		key := [2]string{c.Scenario.Name(), c.Server.Name()}
		counts[key]++
	}
	if len(counts) != len(ss)*len(sv) {
		t.Fatalf("distinct pairs = %d, want %d", len(counts), len(ss)*len(sv))
	}
	for k, n := range counts {
		if n != runs {
			t.Fatalf("pair %v appeared %d times, want %d", k, n, runs)
		}
	}

	// RunIdx must be monotonically non-decreasing (outer loop is runs).
	for i := 1; i < len(cells); i++ {
		if cells[i].RunIdx < cells[i-1].RunIdx {
			t.Fatalf("RunIdx regressed at index %d: %d -> %d",
				i, cells[i-1].RunIdx, cells[i].RunIdx)
		}
	}

	// Within each run, the ordered pair sequence is identical.
	perRun := len(ss) * len(sv)
	for r := 1; r < runs; r++ {
		for j := 0; j < perRun; j++ {
			a := cells[j]
			b := cells[r*perRun+j]
			if a.Scenario.Name() != b.Scenario.Name() ||
				a.Server.Name() != b.Server.Name() {
				t.Fatalf("run %d position %d diverges from run 0: "+
					"got (%s,%s), want (%s,%s)",
					r, j,
					b.Scenario.Name(), b.Server.Name(),
					a.Scenario.Name(), a.Server.Name())
			}
		}
	}
}

func TestScheduleSkipsInapplicablePairs(t *testing.T) {
	// Scenario X only matches servers advertising HTTP2C.
	scenX := &fakeScenario{
		name: "X",
		applicable: func(fs servers.FeatureSet) bool {
			return fs.HTTP2C
		},
	}
	scenY := &fakeScenario{name: "Y"} // always applicable

	sv := []servers.Server{
		&fakeServer{name: "h1only", features: servers.FeatureSet{HTTP1: true}},
		&fakeServer{name: "h2both", features: servers.FeatureSet{HTTP1: true, HTTP2C: true}},
	}
	runs := 2
	cells := Schedule(runs, []scenarios.Scenario{scenX, scenY}, sv)

	// Y against both servers (2) + X against only h2both (1) = 3 pairs × 2 runs = 6.
	if len(cells) != 6 {
		t.Fatalf("len(cells) = %d, want 6", len(cells))
	}
	for _, c := range cells {
		if c.Scenario.Name() == "X" && c.Server.Name() == "h1only" {
			t.Fatalf("inapplicable pair (X, h1only) must not appear")
		}
	}
}
