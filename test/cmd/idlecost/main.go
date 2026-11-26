//go:build linux

// Command idlecost measures what an idle celeris server on a native engine
// costs, and what the idleness costs the requests that follow it (celeris#1381).
// It uses only the exported API of the module it is built in, so the same source
// builds against any revision and gives a before and after by the same method.
//
// For one engine and one worker count it prints, one per line:
//
//	IDLE      process CPU and the wake-ups of every loop thread over a window
//	FIRST     the time to the first response on a new connection after an idle gap
//	SHUTDOWN  the time from Shutdown to Start returning, on an idle server
//
// Loop threads are found by their affinity (a thread pinned to one CPU), and a
// wake-up is a voluntary context switch of that thread (it blocked and was woken),
// read from /proc/self/task/<tid>/status. The CPU is the process's utime plus
// stime from /proc/self/stat over the window, as a percentage of one CPU.
//
// Usage:
//
//	idlecost -engine epoll -workers 3 -window 20s -gaps 1s:5,10s:3,60s:2
//
// -gaps lists "idle:count" pairs for the FIRST lines; empty skips them. The
// label the caller puts on the output (the runner, its CPU count, the revision)
// belongs to the caller: with more workers than CPUs the loops are oversubscribed.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goceleris/celeris"
)

func main() {
	engine := flag.String("engine", "epoll", "epoll, iouring, adaptive or std")
	workers := flag.Int("workers", 0, "celeris.Config.Workers (0: the default)")
	window := flag.Duration("window", 20*time.Second, "the idle measurement window")
	gaps := flag.String("gaps", "", "idle:count pairs for the first-request lines, e.g. 1s:5,10s:3,60s:2")
	flag.Parse()
	if err := run(*engine, *workers, *window, *gaps); err != nil {
		fmt.Fprintln(os.Stderr, "idlecost:", err)
		os.Exit(1)
	}
}

func engineType(name string) (celeris.EngineType, error) {
	switch name {
	case "epoll":
		return celeris.Epoll, nil
	case "iouring":
		return celeris.IOUring, nil
	case "adaptive":
		return celeris.Adaptive, nil
	case "std":
		return celeris.Std, nil
	}
	return 0, fmt.Errorf("unknown engine %q", name)
}

func run(engineName string, workers int, window time.Duration, gaps string) error {
	eng, err := engineType(engineName)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		return err
	}
	s := celeris.New(celeris.Config{Engine: eng, Addr: addr, Workers: workers})
	s.GET("/ping", func(c *celeris.Context) error { return c.String(http.StatusOK, "ok") })
	done := make(chan error, 1)
	go func() { done <- s.Start() }()

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	get := func() (time.Duration, error) {
		t0 := time.Now()
		resp, err := client.Get("http://" + addr + "/ping")
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return time.Since(t0), nil
	}
	ready := false
	for dl := time.Now().Add(15 * time.Second); time.Now().Before(dl); time.Sleep(20 * time.Millisecond) {
		select {
		case err := <-done:
			return fmt.Errorf("the server stopped before it was ready: %v", err)
		default:
		}
		if _, err := get(); err == nil {
			ready = true
			break
		}
	}
	if !ready {
		return fmt.Errorf("the server was not ready in 15 s")
	}
	label := fmt.Sprintf("engine=%s workers=%d cpus=%d", engineName, workers, cpuCount())

	// IDLE: after one warm request and a settle (the loops reach their longest wait).
	time.Sleep(3 * time.Second)
	loops := loopThreads()
	allBefore, loopBefore := threadSwitches(loops)
	cpuBefore := processCPUTicks()
	t0 := time.Now()
	time.Sleep(window)
	elapsed := time.Since(t0)
	allAfter, loopAfter := threadSwitches(loops)
	cpuTicks := processCPUTicks() - cpuBefore
	cpuPct := 100 * float64(cpuTicks) / clockTicks() / elapsed.Seconds()
	var perLoop []string
	var loopSum int64
	for i := range loops {
		d := loopAfter[i] - loopBefore[i]
		loopSum += d
		perLoop = append(perLoop, fmt.Sprintf("%.1f", float64(d)/elapsed.Seconds()))
	}
	otherPerSec := float64(allAfter-allBefore-loopSum) / elapsed.Seconds()
	fmt.Printf("IDLE %s window=%v loop_threads=%d process_cpu_pct=%.3f loop_wakeups_per_s=[%s] other_threads_voluntary_per_s=%.1f\n",
		label, elapsed.Round(time.Millisecond), len(loops), cpuPct, strings.Join(perLoop, " "), otherPerSec)

	// FIRST: the first request on a new connection after an idle gap.
	for _, g := range strings.Split(gaps, ",") {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		gap, count, err := parseGap(g)
		if err != nil {
			return err
		}
		var lat []time.Duration
		for range count {
			time.Sleep(gap)
			d, err := get()
			if err != nil {
				return fmt.Errorf("request after %v idle: %v", gap, err)
			}
			lat = append(lat, d)
		}
		slices.Sort(lat)
		fmt.Printf("FIRST %s idle=%v n=%d min=%v median=%v max=%v\n", label, gap, count,
			lat[0].Round(10*time.Microsecond), lat[len(lat)/2].Round(10*time.Microsecond), lat[len(lat)-1].Round(10*time.Microsecond))
	}

	// SHUTDOWN: an idle server.
	time.Sleep(3 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t1 := time.Now()
	if err := s.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %v", err)
	}
	select {
	case err := <-done:
		fmt.Printf("SHUTDOWN %s idle_shutdown=%v start_err=%v\n", label, time.Since(t1).Round(10*time.Microsecond), err)
	case <-time.After(20 * time.Second):
		return fmt.Errorf("start did not return 20 s after shutdown")
	}
	return nil
}

func parseGap(s string) (time.Duration, int, error) {
	d, n, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("gap %q: want idle:count", s)
	}
	gap, err := time.ParseDuration(d)
	if err != nil {
		return 0, 0, fmt.Errorf("gap %q: %v", s, err)
	}
	count, err := strconv.Atoi(n)
	if err != nil || count < 1 {
		return 0, 0, fmt.Errorf("gap %q: bad count", s)
	}
	return gap, count, nil
}

// readTasks lists the thread ids of this process.
func readTasks() []int {
	ents, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil
	}
	var tids []int
	for _, e := range ents {
		if tid, err := strconv.Atoi(e.Name()); err == nil {
			tids = append(tids, tid)
		}
	}
	return tids
}

// loopThreads returns the threads pinned to one CPU: the loops'.
func loopThreads() []int {
	var out []int
	for _, tid := range readTasks() {
		b, err := os.ReadFile("/proc/self/task/" + strconv.Itoa(tid) + "/status")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
				if v = strings.TrimSpace(v); !strings.ContainsAny(v, "-,") {
					out = append(out, tid)
				}
			}
		}
	}
	return out
}

// threadSwitches returns the voluntary context switches of every thread of the
// process in all, and of each of the given threads in loops, in order.
func threadSwitches(loops []int) (all int64, perLoop []int64) {
	for _, tid := range readTasks() {
		n := voluntary(tid)
		all += n
		if i := slices.Index(loops, tid); i >= 0 {
			for len(perLoop) <= i {
				perLoop = append(perLoop, 0)
			}
			perLoop[i] = n
		}
	}
	for len(perLoop) < len(loops) {
		perLoop = append(perLoop, 0)
	}
	return all, perLoop
}

func voluntary(tid int) int64 {
	b, err := os.ReadFile("/proc/self/task/" + strconv.Itoa(tid) + "/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "voluntary_ctxt_switches:"); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

// processCPUTicks is utime plus stime of the process, in clock ticks.
func processCPUTicks() int64 {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	// The command name is in parentheses and may hold spaces: fields count from after the last ')'.
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	// f[0] is the state (field 3); utime and stime are fields 14 and 15.
	if len(f) < 13 {
		return 0
	}
	u, _ := strconv.ParseInt(f[11], 10, 64)
	st, _ := strconv.ParseInt(f[12], 10, 64)
	return u + st
}

// clockTicks is the kernel's USER_HZ, 100 on every Linux the module supports.
func clockTicks() float64 { return 100 }

// cpuCount is the number of CPUs the process may run on, as the runtime read it
// at start (before any loop thread was pinned).
func cpuCount() int { return runtime.NumCPU() }
