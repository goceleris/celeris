//go:build linux

package platform

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// PinFailure is one engine loop whose PinToCPU failed.
type PinFailure struct {
	CPU int
	Err error
}

// PinFailures collects the pins that failed while an engine's loops started.
// A loop records its failure on its own thread; the engine takes the
// failures after every loop reported ready and logs them once (celeris#909:
// the error used to be dropped, so a loop the kernel refused to pin, by a
// cgroup cpuset or a seccomp policy, ran unpinned without a word).
//
// The zero value is ready to use. The key is whatever identifies the loop.
type PinFailures struct {
	m sync.Map
}

// Record notes that pinning key's thread to cpu failed with err.
func (f *PinFailures) Record(key any, cpu int, err error) {
	f.m.Store(key, PinFailure{CPU: cpu, Err: err})
}

// Take removes and returns the failure recorded for key.
func (f *PinFailures) Take(key any) (PinFailure, bool) {
	v, ok := f.m.LoadAndDelete(key)
	if !ok {
		return PinFailure{}, false
	}
	return v.(PinFailure), true
}

// LogPinOutcome logs, once for an engine, what the plan did: one Info line
// when the plan departs from the plain "i-th allowed CPU" rule (the little
// CPUs it avoids, how many loops it holds and why, celeris#1036; the CPUs the
// rx-port rule left without a loop, with the port and its share, when it did),
// one Debug line with the candidates the rx-port rule passed over or why it
// stepped aside, and one Warn line when loops could not pin and run unpinned.
// On a heterogeneous host whose plan carries a NIC interrupt report
// ([AnnotatePlanWithNICIRQs]) it also names the CPUs that service a NIC
// interrupt vector (the Info line) and, in one Warn line, those of them in the
// allowed set that run no loop, that the rx-port rule did not leave without
// one on purpose, and that are a little CPU or a big one while no vector has a
// loop beside it (celeris#1036); the identical Warn is logged once per logger,
// address and set of CPUs, the repeat is a Debug line. The Info line carries the number of vectors
// judged not to carry traffic (nic_irq_skipped: the ones that never fired and
// the ones under the floor or the share, together) and a Debug line names the
// ones that took a few interrupts. It logs nothing otherwise, and changes
// nothing: no loop is added or moved for an interrupt.
func LogPinOutcome(log *slog.Logger, engine string, plan CPUPlan, failed []PinFailure) {
	if log == nil {
		return
	}
	if plan.Note != "" {
		attrs := []any{"reason", plan.Note,
			"allowed", FormatCPUs(plan.Allowed), "workers", len(plan.CPUs), "requested", plan.Requested,
			"pinned", plan.Pinned()}
		// The rx-port rule (celeris#1036 item 1) names what it did on the same
		// line: the CPUs left without a loop, the port and its share of the
		// packets received since boot.
		if f := plan.RxFree; len(f.Freed) > 0 {
			attrs = append(attrs, "freed_cpus", FormatCPUs(f.Freed), "rx_port", f.Port, "rx_share_pct", f.SharePercent)
		}
		if len(plan.IRQCPUs) > 0 {
			attrs = append(attrs, "nic_irq_cpus", FormatCPUs(plan.IRQCPUs), "irq_took", plan.IRQTook)
		}
		if n := plan.IRQIdle + plan.IRQNegligible; plan.Heterogeneous && n > 0 {
			// Vectors judged not to carry traffic, so that an operator who
			// expected a Warn for one can see why it is missing (the Debug
			// line below has the names).
			attrs = append(attrs, "nic_irq_skipped", n)
		}
		log.Info(engine+": capability-aware CPU pinning", attrs...)
	}
	if len(plan.RxFree.Why) > 0 {
		log.Debug(engine+": rx-port loop placement", "freed", len(plan.RxFree.Freed) > 0,
			"detail", strings.Join(plan.RxFree.Why, "; "))
	}
	if plan.Heterogeneous && len(plan.IRQCPUs) == 0 && plan.IRQReason != "" {
		log.Debug(engine+": nic irq: no vectors found ("+plan.IRQReason+")", "took", plan.IRQTook)
	}
	if plan.Heterogeneous && plan.IRQIdle+plan.IRQNegligible > 0 {
		logIRQSkipped(log, engine, plan)
	}
	if len(plan.IRQUncovered) > 0 {
		if firstIRQWarn(log, plan) {
			logIRQUncovered(log, engine, plan)
		} else {
			log.Debug(engine+": nic irq: Warn already logged for these CPUs on this address",
				"cpus", FormatCPUs(plan.IRQUncovered), "addr", plan.IRQAddr)
		}
	}
	if len(failed) == 0 {
		return
	}
	cpus := make([]int, 0, len(failed))
	for _, f := range failed {
		cpus = append(cpus, f.CPU)
	}
	log.Warn(engine+": loops could not pin their thread and run unpinned",
		"failed", len(failed), "workers", len(plan.CPUs), "cpus", FormatCPUs(cpus),
		"allowed", FormatCPUs(plan.Allowed), "err", failed[0].Err)
}

// logIRQSkipped logs, at Debug, the vectors of the listener's NICs that were
// not judged to carry traffic: how many took no interrupt since boot, and the
// ones that took a few (name, IRQ, CPU, count, and the limit they are under).
// They are never warned about.
func logIRQSkipped(log *slog.Logger, engine string, plan CPUPlan) {
	parts := make([]string, 0, len(plan.IRQSkipped))
	for _, g := range plan.IRQSkipped {
		cpu := "CPU unknown"
		if g.CPU >= 0 {
			cpu = fmt.Sprintf("CPU %d", g.CPU)
		}
		parts = append(parts, fmt.Sprintf("%s (IRQ %d, %s): %s", g.Name, g.IRQ, cpu, g.Why))
	}
	if more := plan.IRQNegligible - len(plan.IRQSkipped); more > 0 {
		parts = append(parts, fmt.Sprintf("%d more", more))
	}
	log.Debug(engine+": nic irq: vectors under the traffic limits are not warned about",
		"idle", plan.IRQIdle, "negligible", plan.IRQNegligible, "detail", strings.Join(parts, "; "),
		"min_share_percent", nicMinSharePercent, "min_interrupts", nicMinInterrupts)
}

// irqWarnKey identifies a Warn: the logger it goes to, the address the report
// was made for and the set of uncovered CPUs. The adaptive engine reports for
// each of its two engines (the second at its first switch, with the same
// logger and address), and the identical Warn is logged once.
type irqWarnKey struct {
	log  *slog.Logger
	addr string
	set  string
}

// irqWarnedMax bounds the memory of the Warns logged: past it the memory is
// cleared, and a Warn that was forgotten is logged again.
const irqWarnedMax = 256

var irqWarned struct {
	sync.Mutex
	seen map[irqWarnKey]struct{}
}

// firstIRQWarn reports whether the Warn for plan has not been logged yet to
// log, and records it. A server restarted in the same process with the same
// logger and address is also taken for a repeat.
func firstIRQWarn(log *slog.Logger, plan CPUPlan) bool {
	k := irqWarnKey{log, plan.IRQAddr, FormatCPUs(plan.IRQUncovered)}
	irqWarned.Lock()
	defer irqWarned.Unlock()
	if _, ok := irqWarned.seen[k]; ok {
		return false
	}
	if irqWarned.seen == nil || len(irqWarned.seen) >= irqWarnedMax {
		irqWarned.seen = make(map[irqWarnKey]struct{})
	}
	irqWarned.seen[k] = struct{}{}
	return true
}

// logIRQUncovered logs the one Warn line for the CPUs of the allowed set that
// service a NIC interrupt vector and run no loop. It is a report: the engine
// starts the same loops it would without the check, and the interrupt is the
// host's, so the line names what the operator can do, not what was done.
func logIRQUncovered(log *slog.Logger, engine string, plan CPUPlan) {
	parts := make([]string, 0, len(plan.IRQUncovered))
	var irqs []string
	total := 0 // the vectors on the uncovered CPUs
	for _, cpu := range plan.IRQUncovered {
		var names []string
		for _, v := range plan.IRQVectors {
			if v.CPU != cpu || plan.freedOnPurpose(v) {
				continue
			}
			total++
			if len(irqs) < 8 {
				irqs = append(irqs, strconv.Itoa(v.IRQ))
			}
			if len(names) < 4 {
				names = append(names, fmt.Sprintf("%s (IRQ %d)", v.Name, v.IRQ))
			}
		}
		if n := countVectorsOn(plan, cpu); n > len(names) {
			names = append(names, fmt.Sprintf("%d more", n-len(names)))
		}
		parts = append(parts, fmt.Sprintf("CPU %d: %s", cpu, strings.Join(names, ", ")))
	}
	loops := slices.Clone(plan.CPUs)
	slices.Sort(loops)
	loops = slices.Compact(loops)
	loops = slices.DeleteFunc(loops, func(c int) bool { return c < 0 })
	path := "/proc/irq/<irq>/smp_affinity_list"
	switch {
	case len(irqs) == 1:
		path = "/proc/irq/" + irqs[0] + "/smp_affinity_list"
	case len(irqs) > 1:
		path += " for each IRQ of " + strings.Join(irqs, ", ")
	}
	if len(irqs) < total {
		path += " and the other vectors named above"
	}
	// The CPUs to offer are the loop CPUs that service no NIC vector: CPU 0 of
	// msr1 carries IRQ 159, and CPUs 6 and 8 the vectors of the receiving
	// port, so a vector moved there stacks two busy vectors on one CPU (and
	// on 6 or 8 the rx-port rule keeps that CPU's loop on the next start,
	// a plan nobody measured).
	free := slices.DeleteFunc(slices.Clone(loops), func(c int) bool { return slices.Contains(plan.IRQCPUs, c) })
	action := "Moving it needs a loop CPU that services no NIC vector, and every loop CPU services a NIC vector."
	if len(free) > 0 {
		action = fmt.Sprintf("If you move it, as root: echo <one of %s> > %s (a loop CPU that services no NIC vector).",
			FormatCPUs(free), path)
	}
	hint := fmt.Sprintf("No loop of this server runs on CPU(s) %s, which service(s) the interrupt(s) above, and none was added; "+
		"the interrupt is the host's. %s One observation on one host, not a guarantee; "+
		"see the README, \"NIC interrupts on big.LITTLE hosts\".",
		FormatCPUs(plan.IRQUncovered), action)
	log.Warn(engine+": NIC interrupt vectors are serviced by CPUs that run no loop",
		"cpus", FormatCPUs(plan.IRQUncovered), "detail", strings.Join(parts, "; "),
		"loop_cpus", FormatCPUs(loops), "allowed", FormatCPUs(plan.Allowed), "hint", hint)
}

// countVectorsOn counts the vectors of the report on cpu that the Warn
// names: not the ones the rx-port rule left there on purpose.
func countVectorsOn(plan CPUPlan, cpu int) int {
	n := 0
	for _, v := range plan.IRQVectors {
		if v.CPU == cpu && !plan.freedOnPurpose(v) {
			n++
		}
	}
	return n
}
