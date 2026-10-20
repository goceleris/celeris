//go:build linux

package iouring

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/engine"
	"github.com/goceleris/celeris/internal/platform"
	"github.com/goceleris/celeris/internal/platform/pintest"
	"github.com/goceleris/celeris/internal/resource"
)

// celeris#1036 item 1: the plan the engine hands the interrupt report carries
// the rx-port rule's RxFree, and the report leaves the CPUs the rule freed on
// purpose out of the Warn. The planner and the interrupt finder are stood in
// for (see TestNICIRQReportAddsNoLoop1036); the finder calls the platform's
// own WithNICIRQs, which is where the exclusion lives.
func TestNICIRQReportLeavesTheRulesFreedCPUs1036(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 2*time.Minute)
		return
	}
	cpus, err := pintest.AllowedCPUs()
	if err != nil || len(cpus) < 1 {
		t.Fatalf("premise: the process has no allowed CPU: %v (%v)", cpus, err)
	}
	const freed, foreign = 1000, 1001 // CPUs of the stand-in plan's allowed set that no loop uses
	for _, tc := range []struct {
		name string
		vecs []platform.NICVector
		warn string // "" for no Warn, else the CPU the Warn names
	}{
		{"only the rule's vector", []platform.NICVector{
			{IRQ: 135, CPU: freed, Name: "enp49s0-8", Dev: "enp49s0", Count: 4_000_000_000}}, ""},
		{"the rule's vector and another port's on a second CPU", []platform.NICVector{
			{IRQ: 135, CPU: freed, Name: "enp49s0-8", Dev: "enp49s0", Count: 4_000_000_000},
			{IRQ: 167, CPU: foreign, Name: "enp1s0-8", Dev: "enp1s0", Count: 2_600_000_000}}, "cpus=1001 "},
	} {
		func() {
			oldPlan, oldNote := planWorkerCPUs, annotateNICIRQs
			defer func() { planWorkerCPUs, annotateNICIRQs = oldPlan, oldNote }()
			const planned = 2
			planWorkerCPUs = func(n int, explicit bool, addr string) platform.CPUPlan {
				loops := make([]int, planned)
				for i := range loops {
					loops[i] = cpus[i%len(cpus)]
				}
				return platform.CPUPlan{CPUs: loops, Allowed: append(slices.Clone(cpus), freed, foreign),
					Little: []int{foreign}, Requested: n, Heterogeneous: true, Note: "test plan",
					RxFree: platform.RxFree{Port: "enp49s0", SharePercent: 73, Freed: []int{freed},
						Vectors: []platform.RxVector{{IRQ: 135, Name: "enp49s0-8", CPU: freed, Count: 4_000_000_000}}}}
			}
			annotateNICIRQs = func(p platform.CPUPlan, addr string) platform.CPUPlan {
				return p.WithNICIRQs(platform.NICIRQs{Vectors: tc.vecs})
			}
			var log lockedBuf
			eng, stop := start909(t, 0, &log)
			defer stop()
			started := planned
			if m := MaxWorkersForMemlock(); m > 0 && m < started {
				started = m
			}
			if got := len(eng.workers); got != started {
				t.Errorf("%s: %d loops started, want %d", tc.name, got, started)
			}
			out := log.String()
			warned := strings.Contains(out, "NIC interrupt vectors are serviced by CPUs that run no loop")
			if warned != (tc.warn != "") {
				t.Errorf("%s: Warn = %v, want %v:\n%s", tc.name, warned, tc.warn != "", out)
			}
			if tc.warn != "" && !strings.Contains(out, tc.warn) {
				t.Errorf("%s: the Warn lacks %q:\n%s", tc.name, tc.warn, out)
			}
			if strings.Contains(out, "CPU 1000: ") {
				t.Errorf("%s: the Warn names CPU 1000, which the rule freed on purpose:\n%s", tc.name, out)
			}
			for _, w := range []string{"freed_cpus=1000", "nic_irq_cpus="} {
				if !strings.Contains(out, w) {
					t.Errorf("%s: the Info line lacks %q:\n%s", tc.name, w, out)
				}
			}
		}()
	}
}

// The memlock cut is Truncate, which keeps the plan's RxFree: the CPUs the rule
// freed stay unwarned after the cut, and a CPU whose loop the cut dropped
// (here CPU 1, which services IRQ 167) is named. The planner and the interrupt
// finder are stood in for.
func TestMemlockCutKeepsTheRulesFreedCPUs1036(t *testing.T) {
	loops := []int{0, 1, 7, 9, 10, 11}
	oldPlan, oldCeiling := planWorkerCPUs, memlockCeiling
	t.Cleanup(func() { planWorkerCPUs, memlockCeiling = oldPlan, oldCeiling })
	planWorkerCPUs = func(n int, explicit bool, addr string) platform.CPUPlan {
		return platform.CPUPlan{CPUs: slices.Clone(loops), Allowed: []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11},
			Little: []int{2, 3, 4, 5}, Requested: n, Heterogeneous: true, Note: "test plan: 6 loops",
			RxFree: platform.RxFree{Port: "enp49s0", SharePercent: 73, Freed: []int{6, 8}, Vectors: []platform.RxVector{
				{IRQ: 135, Name: "enp49s0-8", CPU: 8, Count: 4_000_000_000}, {IRQ: 127, Name: "enp49s0-0", CPU: 6, Count: 2_000_000_000}}}}
	}
	vectors := platform.NICIRQs{Vectors: []platform.NICVector{
		{IRQ: 127, CPU: 6, Name: "enp49s0-0", Dev: "enp49s0", Count: 2_000_000_000},
		{IRQ: 135, CPU: 8, Name: "enp49s0-8", Dev: "enp49s0", Count: 4_000_000_000},
		{IRQ: 167, CPU: 1, Name: "enp1s0-8", Dev: "enp1s0", Count: 2_600_000_000},
	}}
	for _, tc := range []struct {
		ceiling   int
		want      []int
		uncovered []int
	}{
		{-1, loops, nil},
		{6, loops, nil},
		{3, loops[:3], nil},
		{1, loops[:1], []int{1}},
	} {
		memlockCeiling = func() int { return tc.ceiling }
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		plan := planWithinMemlock(12, false, "", log)
		if !slices.Equal(plan.CPUs, tc.want) {
			t.Errorf("ceiling %d: CPUs %v, want %v", tc.ceiling, plan.CPUs, tc.want)
		}
		if !slices.Equal(plan.RxFree.Freed, []int{6, 8}) {
			t.Errorf("ceiling %d: the cut plan lost the rule's freed CPUs: %v", tc.ceiling, plan.RxFree.Freed)
		}
		note := plan.WithNICIRQs(vectors)
		if !slices.Equal(note.IRQUncovered, tc.uncovered) {
			t.Errorf("ceiling %d: uncovered %v, want %v", tc.ceiling, note.IRQUncovered, tc.uncovered)
		}
		buf.Reset()
		platform.LogPinOutcome(log, "io_uring", note, nil)
		out := buf.String()
		if warned := strings.Contains(out, "level=WARN"); warned != (len(tc.uncovered) > 0) {
			t.Errorf("ceiling %d: Warn = %v, want %v:\n%s", tc.ceiling, warned, len(tc.uncovered) > 0, out)
		}
		for _, c := range []int{6, 8} {
			if strings.Contains(out, fmt.Sprintf("CPU %d: ", c)) {
				t.Errorf("ceiling %d: the Warn names CPU %d, which the rule freed on purpose:\n%s", tc.ceiling, c, out)
			}
		}
	}
}

// startAt is start909 with a logger that logs from level up.
func startAt(t *testing.T, workers int, log io.Writer, level slog.Level) (*Engine, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	e, cancel, done := startRingRetried662(t, func() (*Engine, error) {
		return New(resource.Config{
			Addr:      addr,
			Protocol:  engine.HTTP1,
			Resources: resource.Resources{Workers: workers},
			Logger:    slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: level})),
		}, respondingHandler{})
	})
	return e, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Listen: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the io_uring engine did not stop within 15s")
		}
	}
}

// The interrupt report reads files, so it is made only when the logger would
// show something of it: its Warn is the lowest level it adds to the pin line.
func TestNICIRQReportIsSkippedWhenNothingWouldShow1036(t *testing.T) {
	if !pintest.InOwnProcess(t) {
		pintest.RunInOwnProcess(t, 2*time.Minute)
		return
	}
	cpus, err := pintest.AllowedCPUs()
	if err != nil || len(cpus) < 1 {
		t.Fatalf("premise: the process has no allowed CPU: %v (%v)", cpus, err)
	}
	for _, tc := range []struct {
		name  string
		level slog.Level
		calls int
	}{
		{"errors only", slog.LevelError, 0},
		{"warnings", slog.LevelWarn, 1},
		{"info", slog.LevelInfo, 1},
	} {
		func() {
			oldPlan, oldNote := planWorkerCPUs, annotateNICIRQs
			defer func() { planWorkerCPUs, annotateNICIRQs = oldPlan, oldNote }()
			planWorkerCPUs = func(n int, explicit bool, addr string) platform.CPUPlan {
				return platform.CPUPlan{CPUs: []int{cpus[0]}, Allowed: slices.Clone(cpus), Requested: n, Heterogeneous: true, Note: "test plan"}
			}
			calls := 0
			annotateNICIRQs = func(p platform.CPUPlan, addr string) platform.CPUPlan { calls++; return p }
			var log lockedBuf
			_, stop := startAt(t, 0, &log, tc.level)
			defer stop()
			if calls != tc.calls {
				t.Errorf("%s: the report ran %d times, want %d", tc.name, calls, tc.calls)
			}
		}()
	}
}
