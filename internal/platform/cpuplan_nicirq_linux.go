//go:build linux

package platform

import (
	"context"
	"log/slog"
	"slices"
	"time"
)

// AnnotatePlanWithNICIRQs records on p which CPUs service the interrupt vectors
// of the NIC the listener bound to addr is reachable through, and which of them
// run no loop of p (celeris#1036), leaving out the CPUs the rx-port rule freed
// on purpose (see [CPUPlan.WithNICIRQs]). It reports; it never changes the plan:
// CPUs, Requested, Allowed, Little, Note and RxFree come back as they went in,
// so the engine starts the loops the planner (and the RLIMIT_MEMLOCK cut) chose
// and the NIC interrupts add or move none. LogPinOutcome logs the result once.
//
// It runs once, when an engine has started its loops. It reads nothing and
// returns p on a host whose CPUs are alike (all amd64) and on a plan with no
// loops. A file it cannot read or a format it does not know leaves p as it
// was: it never fails Listen.
func AnnotatePlanWithNICIRQs(p CPUPlan, addr string) CPUPlan {
	return annotatePlanNIC(p, addr, defaultNICSource())
}

// NICIRQReportWanted reports whether log would show anything the interrupt
// report adds to the pin line: its Warn is the lowest level it logs at that
// the operator acts on, and Info and Debug are below it. The engines make the
// report (a bounded number of file reads) only then.
func NICIRQReportWanted(log *slog.Logger) bool {
	return log != nil && log.Enabled(context.Background(), slog.LevelWarn)
}

// annotatePlanNIC is AnnotatePlanWithNICIRQs over a source. The gate (a
// heterogeneous plan with loops) stands before the first read.
func annotatePlanNIC(p CPUPlan, addr string, src nicSource) CPUPlan {
	if !p.Heterogeneous || len(p.CPUs) == 0 {
		return p
	}
	t0 := time.Now()
	irq := detectNICIRQs(addr, src)
	irq.Took = time.Since(t0)
	q := p.WithNICIRQs(irq)
	q.IRQAddr = addr
	return q
}

// WithNICIRQs returns p with the NIC interrupt report of irq: the serviced
// CPUs, and, in IRQUncovered, those of them in the allowed set that run no
// loop of p, that the rx-port rule did not leave without one on purpose, and
// that are either a little CPU or a big one when no reported vector has a loop
// beside it. A serviced CPU outside the allowed set is in IRQCPUs only: this
// process cannot run a loop there. p is returned unchanged unless it is
// heterogeneous, and its loops are never changed.
//
// The rx-port rule ([RxFree], celeris#1036 item 1) leaves the big CPUs that
// service the busy vectors of the receiving port without a loop, and that plan
// is measured: six loops on 0, 1, 7, 9, 10 and 11 against eight, with the
// vectors of CPUs 6 and 8 left unserved by a loop. A vector the rule listed in
// RxFree.Vectors, on a CPU in RxFree.Freed, is therefore not a reason to warn;
// any other vector on such a CPU still is (the exclusion is per vector, not per
// CPU). The plan's RxFree survives the memlock cut ([CPUPlan.Truncate]).
func (p CPUPlan) WithNICIRQs(irq NICIRQs) CPUPlan {
	if !p.Heterogeneous {
		return p
	}
	q := p
	q.IRQReason = irq.Reason
	q.IRQTook = irq.Took
	q.IRQIdle, q.IRQNegligible, q.IRQSkipped = irq.Idle, irq.Negligible, slices.Clone(irq.Skipped)
	q.IRQVectors, q.IRQCPUs, q.IRQUncovered = nil, nil, nil
	for _, v := range irq.Vectors {
		if v.CPU < 0 {
			continue
		}
		q.IRQVectors = append(q.IRQVectors, v)
		if !slices.Contains(q.IRQCPUs, v.CPU) {
			q.IRQCPUs = append(q.IRQCPUs, v.CPU)
		}
	}
	// The vectors the rx-port rule stepped aside over (busy by its floor of
	// 1000 interrupts, on a CPU with no loop) count whatever their share: a
	// wrapped 32-bit counter can take the vector that matters under the share
	// test while the rule still sees it busy.
	for _, r := range p.RxFree.NoLoop {
		if r.CPU < 0 || slices.ContainsFunc(q.IRQVectors, func(v NICVector) bool { return v.IRQ == r.IRQ }) {
			continue
		}
		q.IRQVectors = append(q.IRQVectors, NICVector{IRQ: r.IRQ, CPU: r.CPU, Name: r.Name, Count: r.Count})
		if !slices.Contains(q.IRQCPUs, r.CPU) {
			q.IRQCPUs = append(q.IRQCPUs, r.CPU)
		}
	}
	slices.Sort(q.IRQCPUs)
	// The premise of the rx-port rule (steps 4 and 7): a loop stays beside a
	// busy vector. When one does, a big CPU without a loop is the state the
	// rule measured fine (CPUs 6 and 8 of msr1 service enp49s0's vectors and
	// run none while CPU 1 keeps a loop beside IRQ 167), so only a little CPU,
	// where no loop runs by design, is a reason to warn; when no vector has a
	// loop beside it, any CPU without one is.
	beside := slices.ContainsFunc(q.IRQVectors, func(v NICVector) bool { return slices.Contains(q.CPUs, v.CPU) })
	for _, c := range q.IRQCPUs {
		if !slices.Contains(q.Allowed, c) || slices.Contains(q.CPUs, c) {
			continue
		}
		if beside && !slices.Contains(q.Little, c) {
			continue
		}
		if slices.ContainsFunc(q.IRQVectors, func(v NICVector) bool { return v.CPU == c && !p.freedOnPurpose(v) }) {
			q.IRQUncovered = append(q.IRQUncovered, c)
		}
	}
	return q
}

// freedOnPurpose reports whether the rx-port rule left v's CPU without a loop
// because of v: the rule listed this vector (same IRQ, same CPU) among the
// busy vectors of the receiving port on the CPUs it freed.
func (p CPUPlan) freedOnPurpose(v NICVector) bool {
	return slices.Contains(p.RxFree.Freed, v.CPU) &&
		slices.ContainsFunc(p.RxFree.Vectors, func(r RxVector) bool { return r.IRQ == v.IRQ && r.CPU == v.CPU })
}
