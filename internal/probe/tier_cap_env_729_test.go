package probe

import (
	"reflect"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
)

// Tests for celeris#729: the CELERIS_MAX_IOURING_TIER value rules the README
// row states. The names optional, high and base cap at that tier, any other
// non-empty value (a typo, a different case) caps at none, and an empty value
// is the same as unset: no cap. The cap only lowers, and the kernel version
// is left as detected.

func TestParseTierName729(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want engine.Tier
	}{
		{"optional", engine.Optional},
		{"high", engine.High},
		{"base", engine.Base},
		{"none", engine.None},
		{"hgih", engine.None},
		{"High", engine.None},
		{"NONE", engine.None},
		{" high", engine.None},
		{"0", engine.None},
	} {
		if got := parseTierName(tc.in); got != tc.want {
			t.Errorf("parseTierName(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestProbeTierCapEnv729 runs the real Probe. An empty value must leave the
// profile exactly as ProbeWith detects it (no cap); the typo must cap at none
// and leave the kernel version alone. The host's own tier decides whether the
// cap is visible: a host that probes as none is unchanged by every value, so
// the typo case compares against the detected profile with the tier cleared
// instead of asserting a drop.
func TestProbeTierCapEnv729(t *testing.T) {
	detected := ProbeWith(defaultProber())

	t.Setenv("CELERIS_MAX_IOURING_TIER", "")
	if got := Probe(); !reflect.DeepEqual(got, detected) {
		t.Errorf("an empty CELERIS_MAX_IOURING_TIER changed the profile:\n got %+v\nwant %+v", got, detected)
	}

	t.Setenv("CELERIS_MAX_IOURING_TIER", "hgih")
	got := Probe()
	want := capIOUringTier(detected, engine.None)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a typo in CELERIS_MAX_IOURING_TIER did not cap at none:\n got %+v\nwant %+v", got, want)
	}
	if got.IOUringTier != engine.None {
		t.Errorf("IOUringTier = %v, want none", got.IOUringTier)
	}
	if got.KernelMajor != detected.KernelMajor || got.KernelMinor != detected.KernelMinor || got.KernelVersion != detected.KernelVersion {
		t.Errorf("the cap changed the detected kernel version: got %d.%d (%q), detected %d.%d (%q)",
			got.KernelMajor, got.KernelMinor, got.KernelVersion, detected.KernelMajor, detected.KernelMinor, detected.KernelVersion)
	}
}

// TestCapIOUringTierOnlyLowers729: "Caps ... below what the kernel supports":
// a cap above the detected tier changes nothing.
func TestCapIOUringTierOnlyLowers729(t *testing.T) {
	p := engine.CapabilityProfile{KernelMajor: 5, KernelMinor: 15, IOUringTier: engine.Base, LinkedSQEs: true}
	if got := capIOUringTier(p, engine.Optional); !reflect.DeepEqual(got, p) {
		t.Errorf("a cap above the detected tier changed the profile:\n got %+v\nwant %+v", got, p)
	}
	hi := engine.CapabilityProfile{KernelMajor: 6, KernelMinor: 12, IOUringTier: engine.Optional, MultishotRecv: true, SendZC: true}
	if got := capIOUringTier(hi, engine.Base); got.IOUringTier != engine.Base || got.MultishotRecv || got.SendZC {
		t.Errorf("a cap at base left %+v", got)
	}
}
