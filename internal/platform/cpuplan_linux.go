//go:build linux

package platform

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// littleCapacityPercent is the capacity, as a percentage of the largest
// capacity among the allowed CPUs, below which a CPU counts as "little": a
// CPU is little when its capacity is under half of the biggest one's.
// Capacities of one core type are not equal (the Cortex-A720s of an msr1 host
// read 866 to 1024), so the test is a threshold and never an equality.
const littleCapacityPercent = 50

// CPUPlan is the CPU each engine loop pins its thread to, decided once when
// the engine starts (celeris#909).
type CPUPlan struct {
	// CPUs has one entry per worker: the CPU that worker pins itself to, or
	// -1 if the worker must run unpinned.
	CPUs []int
	// Allowed is the process's CPU set (sched_getaffinity) the plan was
	// drawn from, ascending.
	Allowed []int
	// Little is the allowed CPUs the plan keeps loops off, ascending. Empty
	// unless Heterogeneous.
	Little []int
	// Heterogeneous reports a capacity-asymmetric allowed set (big.LITTLE).
	Heterogeneous bool
	// Note is one line that says why the plan departs from the plain
	// "i-th allowed CPU" rule, for the engine to log once. Empty for a
	// homogeneous host.
	Note string
}

// Pinned returns how many workers of the plan are pinned.
func (p CPUPlan) Pinned() int {
	n := 0
	for _, c := range p.CPUs {
		if c >= 0 {
			n++
		}
	}
	return n
}

// PlanWorkerCPUs chooses the CPU of each of numWorkers engine loops.
//
// The candidates are the members of the calling thread's allowed CPU set
// (sched_getaffinity(0): the mask of the thread that calls Listen, which is
// the process's mask unless something narrowed that one thread, such as a
// server started from a goroutine locked to a thread the application pinned),
// not the indices 0..NumCPU-1: a process restricted by
// taskset, sched_setaffinity or a cgroup cpuset keeps its loops inside the
// restriction (celeris#909). On a host whose allowed CPUs differ in capacity
// (arm64 big.LITTLE; /sys/devices/system/cpu/cpuN/cpu_capacity, or the CPU
// parts in /proc/cpuinfo when the kernel exports no capacity), loops pin only
// to the CPUs that are not little and the loops left over run unpinned: a loop
// pinned to a Cortex-A520 of an msr1 host stalled its connections for 5 s and
// more, a loop on an A720 or an unpinned one never did. Where the CPUs are
// alike the i-th worker gets the i-th allowed CPU, wrapping. On a host with
// several NUMA nodes the CPUs are taken from the nodes the allowed set covers
// in turn, and every allowed CPU gets a loop before any gets a second.
//
// Any read error means "homogeneous"; the plan then is what a host without
// the information gets.
func PlanWorkerCPUs(numWorkers int) CPUPlan {
	src := planSource{
		allowed:  schedAllowedCPUs,
		cpuDir:   "/sys/devices/system/cpu",
		cpuinfo:  "/proc/cpuinfo",
		nodeCPUs: nil,
	}
	if n := DetectNUMA().NumNodes; n > 1 {
		src.nodeCPUs = readNodeCPUs(n)
	}
	return planWorkerCPUs(numWorkers, src)
}

// planSource is where planWorkerCPUs reads the machine from; the tests point
// it at a fake.
type planSource struct {
	allowed  func() ([]int, error) // the process's allowed CPUs
	cpuDir   string                // /sys/devices/system/cpu
	cpuinfo  string                // /proc/cpuinfo
	nodeCPUs [][]int               // per NUMA node CPU lists; nil for one node
}

// schedAllowedCPUs returns the members of the calling thread's CPU mask.
func schedAllowedCPUs() ([]int, error) {
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		return nil, err
	}
	var out []int
	for c := 0; c < len(set)*64; c++ {
		if set.IsSet(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("sched_getaffinity returned an empty set")
	}
	return out, nil
}

func planWorkerCPUs(numWorkers int, src planSource) CPUPlan {
	if numWorkers < 0 {
		numWorkers = 0
	}
	allowed, err := src.allowed()
	if err != nil || len(allowed) == 0 {
		// No mask to draw from: the count Go saw at start, as before.
		n := max(runtime.NumCPU(), 1)
		allowed = make([]int, n)
		for i := range allowed {
			allowed[i] = i
		}
	}
	allowed = slices.Clone(allowed)
	slices.Sort(allowed)
	allowed = slices.Compact(allowed)

	plan := CPUPlan{Allowed: allowed}
	little, capNote := littleCPUs(allowed, src)
	if len(little) == 0 {
		plan.CPUs = spread(numWorkers, allowed, src.nodeCPUs)
		return plan
	}

	big := make([]int, 0, len(allowed)-len(little))
	for _, c := range allowed {
		if !slices.Contains(little, c) {
			big = append(big, c)
		}
	}
	plan.Heterogeneous = true
	plan.Little = little
	pinned := min(numWorkers, len(big))
	plan.CPUs = make([]int, numWorkers)
	copy(plan.CPUs, big[:pinned])
	for i := pinned; i < numWorkers; i++ {
		plan.CPUs[i] = -1
	}
	plan.Note = fmt.Sprintf("heterogeneous CPU capacities (%s): the little CPUs %s are avoided; "+
		"%d of %d loops pinned to the other allowed CPUs %s, %d left unpinned",
		capNote, FormatCPUs(little), pinned, numWorkers, FormatCPUs(big), numWorkers-pinned)
	return plan
}

// spread picks numWorkers CPUs out of cands, taking the CPUs of the host's
// NUMA nodes in turn (interleaveNodes) and wrapping around the whole order
// only when there are more workers than CPUs, so that no CPU holds a second
// loop while another one holds none. nodes may be nil.
func spread(numWorkers int, cands []int, nodes [][]int) []int {
	cpus := make([]int, numWorkers)
	if len(cands) == 0 {
		for i := range cpus {
			cpus[i] = -1
		}
		return cpus
	}
	order := interleaveNodes(cands, nodes)
	for i := range cpus {
		cpus[i] = order[i%len(order)]
	}
	return cpus
}

// interleaveNodes returns every member of cands once, ordered round-robin
// across the NUMA nodes the set covers: the first CPU of each node, then the
// second of each, skipping a node that has no CPU left. Only the nodes whose
// CPU list meets cands take part (a cpuset or taskset that covers one node, or
// covers them unevenly, leaves the others out rather than handing their turns
// to CPUs another node already used; celeris#909). Members of cands that no
// node lists come last. With fewer than two nodes in play the order is cands.
func interleaveNodes(cands []int, nodes [][]int) []int {
	if len(nodes) < 2 {
		return cands
	}
	inSet := make(map[int]bool, len(cands))
	for _, c := range cands {
		inSet[c] = true
	}
	var lists [][]int
	seen := make(map[int]bool, len(cands))
	for _, list := range nodes {
		var l []int
		for _, c := range list {
			if inSet[c] && !seen[c] {
				seen[c] = true
				l = append(l, c)
			}
		}
		if len(l) > 0 {
			lists = append(lists, l)
		}
	}
	if len(lists) < 2 {
		return cands
	}
	order := make([]int, 0, len(cands))
	for round := 0; len(order) < len(seen); round++ {
		for _, l := range lists {
			if round < len(l) {
				order = append(order, l[round])
			}
		}
	}
	for _, c := range cands {
		if !seen[c] {
			order = append(order, c)
		}
	}
	return order
}

// littleCPUs returns the members of allowed that are little, and a short
// description of the evidence. It returns nothing when the CPUs are alike or
// when it cannot tell (any read error counts as "alike").
func littleCPUs(allowed []int, src planSource) ([]int, string) {
	if caps, ok := readCapacities(src.cpuDir, allowed); ok {
		top := 0
		for _, c := range allowed {
			top = max(top, caps[c])
		}
		var little []int
		for _, c := range allowed {
			if caps[c]*100 < top*littleCapacityPercent {
				little = append(little, c)
			}
		}
		if len(little) == 0 || len(little) == len(allowed) {
			return nil, ""
		}
		return little, fmt.Sprintf("cpu_capacity below %d%% of the largest, %d", littleCapacityPercent, top)
	}
	if little, ok := littleByCPUPart(src.cpuinfo, allowed); ok {
		return little, "no cpu_capacity, /proc/cpuinfo CPU parts"
	}
	return nil, ""
}

// readCapacities reads <cpuDir>/cpuN/cpu_capacity of every allowed CPU. ok is
// false unless every one of them has a positive capacity.
func readCapacities(cpuDir string, allowed []int) (caps map[int]int, ok bool) {
	caps = make(map[int]int, len(allowed))
	for _, c := range allowed {
		b, err := os.ReadFile(cpuDir + "/cpu" + strconv.Itoa(c) + "/cpu_capacity")
		if err != nil {
			return nil, false
		}
		v, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || v <= 0 {
			return nil, false
		}
		caps[c] = v
	}
	return caps, len(caps) > 0
}

// littleParts are the Arm CPU implementer/part pairs of in-order efficiency
// cores whose capacity is far below half of the performance cores they are
// paired with: Cortex-A35, A53, A55, A510 and A520. It is the fallback for a
// kernel that exports no cpu_capacity; it classifies a CPU as little only if
// another allowed CPU is of a part not in the list.
var littleParts = map[string]bool{
	"0x41:0xd04": true, // Cortex-A35
	"0x41:0xd03": true, // Cortex-A53
	"0x41:0xd05": true, // Cortex-A55
	"0x41:0xd46": true, // Cortex-A510
	"0x41:0xd80": true, // Cortex-A520
}

// littleByCPUPart classifies the allowed CPUs by the "CPU implementer" and
// "CPU part" lines of /proc/cpuinfo. It reports ok only when every allowed CPU
// has both lines and the allowed set mixes known-little with other parts.
func littleByCPUPart(path string, allowed []int) (little []int, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	impl := map[int]string{}
	part := map[int]string{}
	cur := -1
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "processor":
			n, err := strconv.Atoi(v)
			if err != nil {
				cur = -1
			} else {
				cur = n
			}
		case "CPU implementer":
			if cur >= 0 {
				impl[cur] = strings.ToLower(v)
			}
		case "CPU part":
			if cur >= 0 {
				part[cur] = strings.ToLower(v)
			}
		}
	}
	if sc.Err() != nil {
		return nil, false
	}
	other := 0
	for _, c := range allowed {
		i, p := impl[c], part[c]
		if i == "" || p == "" {
			return nil, false
		}
		if littleParts[i+":"+p] {
			little = append(little, c)
		} else {
			other++
		}
	}
	if len(little) == 0 || other == 0 {
		return nil, false
	}
	return little, true
}

// FormatCPUs renders a CPU list the way the kernel does ("0-1,6-11").
func FormatCPUs(cpus []int) string {
	s := slices.Clone(cpus)
	slices.Sort(s)
	var b strings.Builder
	for i := 0; i < len(s); {
		j := i
		for j+1 < len(s) && s[j+1] == s[j]+1 {
			j++
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		if j == i {
			b.WriteString(strconv.Itoa(s[i]))
		} else {
			b.WriteString(strconv.Itoa(s[i]) + "-" + strconv.Itoa(s[j]))
		}
		i = j + 1
	}
	return b.String()
}
