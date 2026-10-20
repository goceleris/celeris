//go:build linux

package platform

import (
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

// annotatePlanNIC is AnnotatePlanWithNICIRQs over a source. The gate (a
// heterogeneous plan with loops) stands before the first read.
func annotatePlanNIC(p CPUPlan, addr string, src nicSource) CPUPlan {
	if !p.Heterogeneous || len(p.CPUs) == 0 {
		return p
	}
	t0 := time.Now()
	irq := detectNICIRQs(addr, src)
	irq.Took = time.Since(t0)
	return p.WithNICIRQs(irq)
}

// WithNICIRQs returns p with the NIC interrupt report of irq: the serviced
// CPUs, and, in IRQUncovered, those of them in the allowed set that run no
// loop of p and that the rx-port rule did not leave without one on purpose. A
// serviced CPU outside the allowed set is in IRQCPUs only: this process cannot
// run a loop there. p is returned unchanged unless it is heterogeneous, and
// its loops are never changed.
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
	slices.Sort(q.IRQCPUs)
	for _, c := range q.IRQCPUs {
		if !slices.Contains(q.Allowed, c) || slices.Contains(q.CPUs, c) {
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
