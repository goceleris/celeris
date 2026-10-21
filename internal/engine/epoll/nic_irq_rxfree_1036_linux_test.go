//go:build linux

package epoll

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	celerisengine "github.com/goceleris/celeris/internal/engine"
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
			if got := len(eng.loops); got != planned {
				t.Errorf("%s: %d loops started, the planner's plan holds %d", tc.name, got, planned)
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

// startAt is start909 with a logger that logs from level up.
func startAt(t *testing.T, workers int, log *lockedBuf, level slog.Level) (*Engine, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	eng, err := New(resource.Config{
		Addr:      addr,
		Protocol:  celerisengine.HTTP1,
		Resources: resource.Resources{Workers: workers},
		Logger:    slog.New(slog.NewTextHandler(log, &slog.HandlerOptions{Level: level})),
	}, respondingHandler{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- eng.Listen(ctx) }()
	for deadline := time.Now().Add(10 * time.Second); eng.Addr() == nil; {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the epoll engine did not bind within 10s")
		}
		time.Sleep(time.Millisecond)
	}
	return eng, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Listen: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the epoll engine did not stop within 15s")
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
