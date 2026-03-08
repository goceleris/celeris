package engine

import "testing"

func TestTierString(t *testing.T) {
	tests := []struct {
		tier Tier
		want string
	}{
		{None, "none"},
		{Base, "base"},
		{Mid, "mid"},
		{High, "high"},
		{Optional, "optional"},
		{Tier(255), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.tier.String(); got != tt.want {
			t.Errorf("Tier(%d).String() = %q, want %q", tt.tier, got, tt.want)
		}
	}
}

func TestTierAvailable(t *testing.T) {
	if None.Available() {
		t.Error("None.Available() = true, want false")
	}
	for _, tier := range []Tier{Base, Mid, High, Optional} {
		if !tier.Available() {
			t.Errorf("%v.Available() = false, want true", tier)
		}
	}
}

func TestTierOrdering(t *testing.T) {
	ordered := []Tier{None, Base, Mid, High, Optional}
	for i := 0; i < len(ordered)-1; i++ {
		if ordered[i] >= ordered[i+1] {
			t.Errorf("expected %v < %v", ordered[i], ordered[i+1])
		}
	}
}
