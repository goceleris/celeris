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
	"time"
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
	// CPUs has one entry per loop the engine starts: the CPU that loop pins
	// itself to. Its length is the loop count: on a host whose allowed CPUs
	// differ in capacity it can be smaller than the number of workers asked
	// for, and the engine starts len(CPUs) loops, not the number it asked for
	// (celeris#1036). The planner never leaves an entry at -1 ("run
	// unpinned"); an engine still honours a -1 it is given, and a pin the
	// kernel refuses leaves that one loop unpinned and is logged.
	CPUs []int
	// Requested is the number of workers the plan was asked for.
	Requested int
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
	// RxFree is what the rx-port rule decided (celeris#1036 item 1). Its
	// Freed list is empty unless the rule took CPUs out of the loop set.
	RxFree RxFree

	// The fields below are the NIC interrupt report (celeris#1036). Only
	// [AnnotatePlanWithNICIRQs] sets them, after the loops are decided, and
	// nothing reads them to decide a loop: CPUs, Requested and Note are the
	// planner's and the memlock cut's alone.
	//
	// IRQCPUs are the CPUs that service an interrupt vector of the NIC the
	// listener is reachable through, ascending. Empty when none was found.
	IRQCPUs []int
	// IRQVectors are the vectors behind IRQCPUs, for the log.
	IRQVectors []NICVector
	// IRQUncovered are the serviced CPUs of the allowed set that run no loop
	// of this plan, ascending, except the ones the rx-port rule left without
	// a loop on purpose (RxFree), and, when a vector has a loop beside it, the
	// big ones (see [CPUPlan.WithNICIRQs]). A warning names them.
	IRQUncovered []int
	// IRQIdle and IRQNegligible count the vectors the detection left out
	// because they took no interrupt, or too few to carry traffic (a floor
	// and a share of the NICs' interrupts since boot, see [DetectNICIRQs]);
	// IRQSkipped describes the first few of the second kind. The Info line
	// carries the sum (nic_irq_skipped: the vectors that never fired and the
	// ones under the limits together, 60 on the host measured, 58 of them
	// idle), a Debug line the detail. Vectors past the 4096 the detection
	// judges are in neither count.
	IRQIdle, IRQNegligible int
	IRQSkipped             []NICSkip
	// IRQReason says why no vector was found, for a Debug line.
	IRQReason string
	// IRQTook is how long the detection took.
	IRQTook time.Duration
	// IRQAddr is the listen address the report was made for; LogPinOutcome
	// logs the Warn once per logger, address and set of uncovered CPUs.
	IRQAddr string
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

// Truncate returns the plan cut to its first n loops, for an engine that can
// fund fewer loops than the plan holds (io_uring under RLIMIT_MEMLOCK). The
// planner fills distinct CPUs before it repeats one, so the first n entries
// are the plan it would have made for n workers. A Note is extended to say how
// many loops start and why, so that it never states a count the engine does
// not run. n at or above the plan's length, or below 1, returns p unchanged.
func (p CPUPlan) Truncate(n int, why string) CPUPlan {
	if n < 1 || n >= len(p.CPUs) {
		return p
	}
	q := p
	q.CPUs = slices.Clone(p.CPUs[:n])
	if q.Note != "" {
		q.Note += fmt.Sprintf("; only %d of the %d planned loops start (%s)", n, len(p.CPUs), why)
	}
	return q
}

// PlanWorkerCPUs chooses the CPU of each engine loop for a request of
// numWorkers workers. explicit reports that numWorkers was set by the user
// (Config.Workers) and not defaulted to GOMAXPROCS.
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
// to the CPUs that are not little, and every loop is pinned: a loop pinned to
// a Cortex-A520 of an msr1 host stalled its connections for 5 s and more, and
// a loop left to float between the CPUs spent about half of its time on the
// little ones, and an interleaved A/B on msr1 put the throughput loss on those
// floating loops, not on the loop count (celeris#1036). So the plan holds
// min(numWorkers, big CPUs) loops, at most one per non-little CPU, and the
// engine starts that many, fewer than the workers it was asked for. A user-set
// numWorkers above the number of non-little CPUs is honoured, never floated:
// the extra loops wrap round-robin over the non-little CPUs, each pinned (two
// loops share a CPU). Where the CPUs are alike the i-th worker gets the i-th
// allowed CPU, wrapping. On a host with several NUMA nodes the CPUs are taken
// from the nodes the allowed set covers in turn, and every allowed CPU gets a
// loop before any gets a second.
//
// Any read error means "homogeneous"; the plan then is what a host without
// the information gets.
//
// PlanWorkerCPUs does not know where the server listens and so never applies
// the rx-port rule; the engines call [PlanWorkerCPUsFor].
func PlanWorkerCPUs(numWorkers int, explicit bool) CPUPlan {
	return PlanWorkerCPUsFor(numWorkers, explicit, "")
}

// PlanWorkerCPUsFor is [PlanWorkerCPUs] for a listener bound to addr
// ("host:port", the concrete address the engine binds). On a big.LITTLE host,
// with Workers not set by the user, it also leaves the loops off the big CPUs
// that service the interrupt vectors of the NIC port that receives the
// traffic (see [RxFree] and cpuplan_rxfree_linux.go): the loop count is never
// raised, and every failure to read the machine gives exactly the plan
// PlanWorkerCPUs makes. An empty addr applies no rule. A homogeneous host and
// an explicit Workers read nothing beyond what PlanWorkerCPUs reads.
func PlanWorkerCPUsFor(numWorkers int, explicit bool, addr string) CPUPlan {
	src := planSource{
		allowed:  schedAllowedCPUs,
		cpuDir:   "/sys/devices/system/cpu",
		cpuinfo:  "/proc/cpuinfo",
		nodeCPUs: nil,
	}
	if addr != "" {
		nic := defaultNICSource()
		src.nic, src.addr = &nic, addr
	}
	if n := DetectNUMA().NumNodes; n > 1 {
		src.nodeCPUs = readNodeCPUs(n)
	}
	return planWorkerCPUs(numWorkers, explicit, src)
}

// planSource is where planWorkerCPUs reads the machine from; the tests point
// it at a fake.
type planSource struct {
	allowed  func() ([]int, error) // the process's allowed CPUs
	cpuDir   string                // /sys/devices/system/cpu
	cpuinfo  string                // /proc/cpuinfo
	nodeCPUs [][]int               // per NUMA node CPU lists; nil for one node
	// nic and addr feed the rx-port rule; a nil nic or an empty addr turns it
	// off.
	nic  *nicSource
	addr string
}

// schedAllowedCPUs returns the members of the calling thread's CPU mask, read
// into a buffer as long as the kernel's (getAffinity), so a host with more than
// 1024 possible CPUs is read whole.
func schedAllowedCPUs() ([]int, error) {
	set, err := getAffinity()
	if err != nil {
		return nil, err
	}
	var out []int
	for c := range setBits(set) {
		if set.IsSet(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("sched_getaffinity returned an empty set")
	}
	return out, nil
}

func planWorkerCPUs(numWorkers int, explicit bool, src planSource) CPUPlan {
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

	plan := CPUPlan{Allowed: allowed, Requested: numWorkers}
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
	// One loop per non-little CPU and no more, unless the user asked for
	// more. A single non-little CPU therefore gives ONE loop by default,
	// below resource.MinWorkers (2, which Validate enforces on a user-set
	// count): deliberate, since a second loop could only share that CPU or run
	// on a little one (celeris#1036). Do not raise it to MinWorkers here.
	//
	// A user-set count above the non-little CPUs is honoured: the extra loops
	// wrap over the same CPUs, still pinned (two loops share a CPU, and a CPU
	// with two loops takes twice the connections of one with one). The big CPUs
	// are spread like any allowed set (NUMA nodes in turn, wrapping only after
	// every CPU holds a loop).
	//
	// The rx-port rule (celeris#1036 item 1) may take CPUs out of the set the
	// loops are drawn from, for the default count only: an explicit
	// Config.Workers is planned as #1049 plans it, and the count is never
	// raised.
	cands := big
	if !explicit && numWorkers > 0 && src.nic != nil && src.addr != "" {
		plan.RxFree = rxFreeFor(numWorkers, src.addr, big, *src.nic)
		if len(plan.RxFree.Freed) > 0 {
			cands = slices.DeleteFunc(slices.Clone(big), func(c int) bool {
				return slices.Contains(plan.RxFree.Freed, c)
			})
		}
	}
	loops := min(numWorkers, len(cands))
	if explicit {
		loops = numWorkers
	}
	plan.CPUs = spread(loops, cands, src.nodeCPUs)
	switch {
	case loops > len(cands):
		plan.Note = fmt.Sprintf("heterogeneous CPU capacities (%s): the little CPUs %s are avoided; "+
			"%d loops (Config.Workers) pinned round-robin over the %d other allowed CPUs %s, "+
			"so some CPUs hold more than one loop, none unpinned",
			capNote, FormatCPUs(little), loops, len(cands), FormatCPUs(cands))
	case loops < numWorkers:
		plan.Note = fmt.Sprintf("heterogeneous CPU capacities (%s): the little CPUs %s are avoided; "+
			"%d loops, one per other allowed CPU %s, none unpinned (not the %d a host of alike CPUs "+
			"would start: the rest would have to run on the little CPUs or float onto them)",
			capNote, FormatCPUs(little), loops, FormatCPUs(cands), numWorkers)
	default:
		// The CPUs the loops use, not the whole set they were drawn from:
		// 5 loops use 5 of 8 non-little CPUs.
		plan.Note = fmt.Sprintf("heterogeneous CPU capacities (%s): the little CPUs %s are avoided; "+
			"%d loops pinned to the CPUs %s of the other allowed CPUs %s, none unpinned",
			capNote, FormatCPUs(little), loops, FormatCPUs(plan.CPUs), FormatCPUs(cands))
	}
	if f := plan.RxFree; len(f.Freed) > 0 {
		plan.Note += "; " + f.Sentence()
	}
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
