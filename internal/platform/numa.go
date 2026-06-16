package platform

import (
	"strconv"
	"strings"
)

// CoreTopology describes the CPU core layout used by the worker formula.
// Logical is the number of hardware threads (runtime.NumCPU); Physical is the
// number of distinct physical cores; SMT is the threads-per-core factor
// (Logical/Physical, >=1, e.g. 2 on a hyperthreaded x86 host). NUMANodes is the
// socket count. DetectCoreTopology fills these from sysfs on Linux and falls
// back to a single-node, no-SMT view elsewhere.
type CoreTopology struct {
	Logical   int
	Physical  int
	SMT       int
	NUMANodes int
}

// parseCPUList parses a Linux CPU list string (e.g., "0-23,48-71") into
// individual CPU IDs.
func parseCPUList(s string) []int {
	if s == "" {
		return nil
	}
	var cpus []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if dashIdx := strings.IndexByte(part, '-'); dashIdx >= 0 {
			lo, err1 := strconv.Atoi(part[:dashIdx])
			hi, err2 := strconv.Atoi(part[dashIdx+1:])
			if err1 != nil || err2 != nil {
				continue
			}
			for cpu := lo; cpu <= hi; cpu++ {
				cpus = append(cpus, cpu)
			}
		} else {
			cpu, err := strconv.Atoi(part)
			if err == nil {
				cpus = append(cpus, cpu)
			}
		}
	}
	return cpus
}
