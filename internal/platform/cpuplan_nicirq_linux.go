//go:build linux

package platform

import (
	"slices"
	"time"
)

// AnnotatePlanWithNICIRQs records on p which CPUs service the interrupt vectors
// of the NIC the listener bound to addr is reachable through, and which of them
// run no loop of p (celeris#1036). It reports; it never changes the plan: CPUs,
// Requested, Allowed, Little and Note come back as they went in, so the engine
// starts the loops the planner (and the RLIMIT_MEMLOCK cut) chose and the NIC
// interrupts add or move none. LogPinOutcome logs the result once.
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
// loop of p. A serviced CPU outside the allowed set is in IRQCPUs only: this
// process cannot run a loop there. p is returned unchanged unless it is
// heterogeneous, and its loops are never changed.
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
		if slices.Contains(q.Allowed, c) && !slices.Contains(q.CPUs, c) {
			q.IRQUncovered = append(q.IRQUncovered, c)
		}
	}
	return q
}
