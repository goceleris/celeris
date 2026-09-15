package release

import (
	"strings"
	"testing"
)

// A checklist that drifts from the checker is worse than no checklist: it
// sends someone to edit a file that no longer carries a version, or omits
// one that does. Adding a stamp must therefore add a line.
func TestChecklistCoversEveryStamp(t *testing.T) {
	body := Checklist("1.6.0")
	for _, s := range Stamps() {
		if !strings.Contains(body, s.Path) {
			t.Errorf("the checklist never mentions %q, which CheckRelease enforces: "+
				"whoever follows it leaves that stamp stale", s.Path)
		}
		if want := strings.TrimLeft(s.Line("1.6.0"), "\t"); !strings.Contains(body, want) {
			t.Errorf("the checklist does not say what %q should become (%q)", s.Path, want)
		}
	}
	for _, sub := range SubModules {
		if !strings.Contains(body, sub+"/v1.6.0") {
			t.Errorf("the checklist never mentions the %s tag, so a missing one "+
				"would not be recognised as a failure", sub)
		}
	}
}

// The other three repositories are the part nothing enforces, which is
// exactly why the checklist is the only record of them.
func TestChecklistNamesTheUnenforcedWork(t *testing.T) {
	body := Checklist("1.6.0")
	for _, want := range []string{
		"loadgen", "version.go", "fallbackVersion",
		"internal/integrationtest/testserver/go.mod",
		"probatorium", "ten modules",
		"docs",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the checklist no longer mentions %q, and nothing else records it", want)
		}
	}
}

// The version must reach every line. A hardcoded one would satisfy the
// coverage test above while telling the reader the wrong number.
func TestChecklistUsesTheVersionItWasGiven(t *testing.T) {
	body := Checklist("2.3.4-rc.1")
	if strings.Contains(body, "1.6.0") {
		t.Error("a 2.3.4-rc.1 checklist leaked a hardcoded 1.6.0")
	}
	for _, want := range []string{
		"VERSION=v2.3.4-rc.1 mage PrepRelease",
		`const Version = "2.3.4-rc.1"`,
		"## What's new in v2.3.4-rc.1",
		"middleware/compress/v2.3.4-rc.1",
		"celeris@v2.3.4-rc.1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the checklist does not carry %q", want)
		}
	}
}

// Milestones name non-release tracks too (Research, Horizon). The workflow
// lets through anything starting with "v", so the version check is what
// stops a bump reminder opening on a milestone with no version to bump.
func TestSemverReAcceptsOnlyRealVersions(t *testing.T) {
	for _, ok := range []string{"1.6.0", "0.0.1", "2.3.4-rc.1", "1.0.0-alpha.2", "1.0.0-beta.10"} {
		if !SemverRe.MatchString(ok) {
			t.Errorf("SemverRe rejected %q", ok)
		}
	}
	for _, bad := range []string{"", "Next", "1.6", "Horizon", "1.6.0.1", "v1.6.0", "1.6.0-rc", "1.6.0-gamma.1"} {
		if SemverRe.MatchString(bad) {
			t.Errorf("SemverRe accepted %q; a milestone named that would open a bump reminder "+
				"with nothing to bump", bad)
		}
	}
}

func TestStripV(t *testing.T) {
	for in, want := range map[string]string{
		"v1.6.0": "1.6.0", "1.6.0": "1.6.0", "  v1.6.0  ": "1.6.0", "": "",
	} {
		if got := StripV(in); got != want {
			t.Errorf("StripV(%q) = %q, want %q", in, got, want)
		}
	}
}
