package platform

import (
	"slices"
	"testing"
)

// The EXPERIMENT-ONLY CPU-list parser (abplan_experiment.go). Runs on every OS.
func TestABPlanOverrideParse(t *testing.T) {
	allowed := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	ok := []struct {
		in   string
		want []int
	}{
		{"0-2,5-11", []int{0, 1, 2, 5, 6, 7, 8, 9, 10, 11}},
		{"0-1,7,9-11", []int{0, 1, 7, 9, 10, 11}},
		{" 11 , 3-4 ,0 ", []int{0, 3, 4, 11}}, // spaces tolerated, output ascending
		{"5", []int{5}},
	}
	for _, c := range ok {
		got, err := abPlanOverride(c.in, allowed)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("abPlanOverride(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	bad := []string{"", "  ", "a", "1-", "-1", "3-1", "1,,2", "0-2,2", "1,1", "12", "0-12", "0,2-x", "1.5"}
	for _, in := range bad {
		if got, err := abPlanOverride(in, allowed); err == nil {
			t.Errorf("abPlanOverride(%q) = %v, nil; want an error", in, got)
		}
	}
	// a CPU outside the allowed set is an error even when well formed
	if _, err := abPlanOverride("0-3", []int{0, 1, 2}); err == nil {
		t.Error("CPU 3 outside the allowed set was accepted")
	}
}
