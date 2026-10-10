//go:build linux

package platform

import (
	"log/slog"
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
// CPUs it avoids), and one Warn line when loops could not pin and run
// unpinned. It logs nothing otherwise.
func LogPinOutcome(log *slog.Logger, engine string, plan CPUPlan, failed []PinFailure) {
	if log == nil {
		return
	}
	if plan.Note != "" {
		log.Info(engine+": capability-aware CPU pinning", "reason", plan.Note,
			"allowed", FormatCPUs(plan.Allowed), "workers", len(plan.CPUs), "pinned", plan.Pinned())
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
