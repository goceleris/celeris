//go:build linux

package pintest

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixtureHost points LittleCPUs and PinnableCPUs at a fake sysfs and a fake
// /proc/cpuinfo of an msr1-like host: CPUs 2-5 are Cortex-A520 (0xd80), the
// others Cortex-A720 (0xd81). withCaps writes the cpu_capacity files.
func fixtureHost(t *testing.T, withCaps bool, parts func(cpu int) (impl, part string)) {
	t.Helper()
	dir := t.TempDir()
	var info strings.Builder
	for c := range 12 {
		if withCaps {
			if err := os.MkdirAll(filepath.Join(dir, fmt.Sprintf("cpu%d", c)), 0o755); err != nil {
				t.Fatal(err)
			}
			v := "1024"
			if c >= 2 && c <= 5 {
				v = "279"
			}
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("cpu%d", c), "cpu_capacity"), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		impl, part := parts(c)
		fmt.Fprintf(&info, "processor\t: %d\nBogoMIPS\t: 50.00\n", c)
		if impl != "" {
			fmt.Fprintf(&info, "CPU implementer\t: %s\n", impl)
		}
		if part != "" {
			fmt.Fprintf(&info, "CPU part\t: %s\n", part)
		}
		info.WriteString("\n")
	}
	infoPath := filepath.Join(dir, "cpuinfo")
	if err := os.WriteFile(infoPath, []byte(info.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	oldDir, oldInfo := cpuSysDir, cpuInfoPath
	cpuSysDir, cpuInfoPath = dir, infoPath
	t.Cleanup(func() { cpuSysDir, cpuInfoPath = oldDir, oldInfo })
}

func msr1Parts(c int) (string, string) {
	if c >= 2 && c <= 5 {
		return "0x41", "0xd80"
	}
	return "0x41", "0xd81"
}

func all12() []int { return []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11} }

// LittleCPUs and PinnableCPUs read the machine the way the planner does,
// capacities first and the CPU parts of /proc/cpuinfo when there are none, so
// the "no pinned loop sits on a little CPU" check of LoopsStayInsideTheMask is
// not vacuous on a kernel that exports no cpu_capacity (celeris#1036).
func TestLittleCPUsReadsCapacitiesThenCPUParts1036(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withCaps   bool
		parts      func(int) (string, string)
		members    []int
		wantLittle []int
		wantPin    int
	}{
		{"capacities", true, msr1Parts, all12(), []int{2, 3, 4, 5}, 8},
		{"capacities, subset", true, msr1Parts, []int{1, 3, 4, 7}, []int{3, 4}, 2},
		{"cpuinfo only", false, msr1Parts, all12(), []int{2, 3, 4, 5}, 8},
		{"cpuinfo only, subset", false, msr1Parts, []int{0, 2, 3, 9}, []int{2, 3}, 2},
		{"cpuinfo only, only little members", false, msr1Parts, []int{2, 3}, nil, 2},
		{"cpuinfo only, a member lacks the lines", false, func(c int) (string, string) {
			if c == 7 {
				return "", ""
			}
			return msr1Parts(c)
		}, all12(), nil, 1},
		{"cpuinfo only, parts differ and none is known little", false, func(c int) (string, string) {
			if c%2 == 0 {
				return "0x41", "0xd81"
			}
			return "0x41", "0xd82"
		}, all12(), nil, 1},
		{"alike, capacities", true, func(int) (string, string) { return "0x41", "0xd81" }, []int{0, 1, 6}, nil, 3},
		{"x86-like: no capacities, no parts", false, func(int) (string, string) { return "", "" }, []int{0, 1, 2}, nil, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixtureHost(t, tc.withCaps, tc.parts)
			got := LittleCPUs(tc.members)
			if !slices.Equal(got, tc.wantLittle) {
				t.Errorf("LittleCPUs(%v) = %v, want %v", tc.members, got, tc.wantLittle)
			}
			if p := PinnableCPUs(tc.members); p != tc.wantPin {
				t.Errorf("PinnableCPUs(%v) = %d, want %d", tc.members, p, tc.wantPin)
			}
		})
	}
}
