//go:build linux

package stallprobe

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// P1 TickGap. One thread per allowed CPU, each pinned (sched_setaffinity,
// runtime.LockOSThread) to its CPU and spinning on the monotonic clock for a
// fixed window; every gap between two consecutive reads above the record
// threshold (20 ms) is kept with its CPU, core type, the CPU's frequency right
// after, and the thread's own scheduler accounting over the gap (time on
// CPU, time runnable but waiting for a CPU, time neither: blocked). A sampler
// reads every CPU's frequency, every thermal zone and /proc/pressure every
// 100 ms. The test FAILS on any gap above 500 ms.
//
// Why this discriminates. A 60 s spin on all CPUs sees, at the same instant:
//   - a gap on every CPU of the spinner process AND of the independent
//     sleeping observer process: the whole machine paused (firmware, a
//     hypervisor, a clock or interrupt problem);
//   - a gap on every CPU of the spinner process only: a process-wide stall (the
//     Go runtime or this address space; celeris#945 was one), not the machine;
//   - a gap on one CPU: that CPU or that thread (preemption by another task,
//     the CPU stuck in a low-power state or at a low frequency). The thread's
//     scheduler accounting says which: "preempted" (runnable, waiting for the
//     CPU), "blocked" (neither running nor runnable), "on-cpu" (ran the whole
//     gap and still read no new time: a hardware or interrupt stall).
//
// The spinner and observer children run with GOGC=off,
// GODEBUG=asyncpreemptoff=1 and GOMAXPROCS=CPUs+2 (set in their environment:
// changing them after start would stop the world), so the Go runtime has
// nothing to schedule or collect during the window and cannot make a gap.
//
// P1b (TestProbeP1bIdleGap) is the same with sleepers only: each CPU is idle
// between 2 ms sleeps, so a stall that needs an idle CPU (a deep idle state,
// a lost timer interrupt) shows where a spinner would hide it.

const (
	p1BucketLo    = 100_000   // 100 us
	p1Sleep       = 2_000_000 // sleeper period, 2 ms
	p1MaxGaps     = 4000      // recorded gaps per CPU
	p1SampleEvery = 100 * time.Millisecond
)

type p1Gap struct {
	StartNs int64 // absolute CLOCK_MONOTONIC
	DurNs   int64 // the gap; for a sleeper, the oversleep
	RunNs   int64 // thread on-CPU time over the gap's window, -1 unknown
	WaitNs  int64 // runnable, waiting for a CPU
	OffNs   int64 // neither (blocked)
	FreqKHz int64 // scaling_cur_freq of the CPU right after, -1 unknown
}

type p1CPU struct {
	CPU     int
	PinErr  string
	Reads   uint64
	B100us  uint64
	B1ms    uint64
	B5ms    uint64
	B20ms   uint64
	MaxNs   int64
	Gaps    []p1Gap
	Dropped int
}

type p1Sample struct {
	T    int64
	Freq []int64 // per observed CPU, kHz
	Temp []int64 // per thermal zone, milli-degrees C
	PSI  []uint64
}

type p1Result struct {
	Kind      string
	Pid       int
	GoMaxProc int
	NumCPU    int
	StartNs   int64
	EndNs     int64
	CPUs      []p1CPU
	Zones     []string
	Samples   []p1Sample
	PSINames  []string
	Notes     []string
}

// ---- child ------------------------------------------------------------------

func p1Child(kind string) (any, error) {
	c := newClk()
	cpus, err := parseCPUList(os.Getenv("CELERIS_PROBE_ARG_CPUS"))
	if err != nil || len(cpus) == 0 {
		return nil, fmt.Errorf("CELERIS_PROBE_ARG_CPUS: %v", err)
	}
	startAbs := envInt64("CELERIS_PROBE_ARG_START", 0)
	secs := envInt64("CELERIS_PROBE_ARG_SECS", 60)
	recNs := envInt64("CELERIS_PROBE_ARG_GAP_NS", 20_000_000)
	endAbs := startAbs + secs*1_000_000_000
	res := &p1Result{Kind: kind, Pid: os.Getpid(), GoMaxProc: runtime.GOMAXPROCS(0), NumCPU: runtime.NumCPU(), StartNs: startAbs, EndNs: endAbs}

	var sam *sampler
	if os.Getenv("CELERIS_PROBE_ARG_SAMPLER") == "1" {
		sam = newSampler(cpus)
		res.Zones, res.PSINames = sam.zones, sam.psiNames
	}

	out := make([]p1CPU, len(cpus))
	var ready, done sync.WaitGroup
	for i, cpu := range cpus {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			if kind == "p1spin" {
				out[i] = p1Spin(c, cpu, startAbs, endAbs, recNs, &ready)
			} else {
				out[i] = p1Sleeper(c, cpu, startAbs, endAbs, recNs, &ready)
			}
		}()
	}
	ready.Wait()
	if sam != nil {
		stop := make(chan struct{})
		sdone := make(chan struct{})
		go func() { defer close(sdone); sam.run(c, startAbs-int64(time.Second), endAbs+int64(time.Second), stop) }()
		defer func() { close(stop); <-sdone; res.Samples = sam.samples }()
	}
	done.Wait()
	res.CPUs = out
	if sam != nil {
		// let the sampler take its closing sample
		time.Sleep(p1SampleEvery + 50*time.Millisecond)
	}
	return res, nil
}

// pinThis locks the calling goroutine to its thread, never unlocking: when the
// goroutine ends the runtime ends the thread instead of reusing it pinned
// (the lesson of celeris#905).
func pinThis(cpu int) string {
	runtime.LockOSThread()
	m := maskOf([]int{cpu})
	if err := unix.SchedSetaffinity(0, &m); err != nil {
		return err.Error()
	}
	return ""
}

type schedFile struct{ fd int }

func openSched() schedFile {
	fd, err := unix.Open(fmt.Sprintf("/proc/self/task/%d/schedstat", unix.Gettid()), unix.O_RDONLY, 0)
	if err != nil {
		return schedFile{-1}
	}
	return schedFile{fd}
}

// read returns on-CPU ns and runqueue-wait ns of the thread, or -1, -1.
func (s schedFile) read(buf []byte) (run, wait int64) {
	if s.fd < 0 {
		return -1, -1
	}
	n, err := unix.Pread(s.fd, buf, 0)
	if err != nil || n <= 0 {
		return -1, -1
	}
	var f [2]int64
	k, v := 0, int64(0)
	have := false
	for _, ch := range buf[:n] {
		switch {
		case ch >= '0' && ch <= '9':
			v = v*10 + int64(ch-'0')
			have = true
		default:
			if have && k < 2 {
				f[k] = v
				k++
			}
			v, have = 0, false
			if k >= 2 {
				return f[0], f[1]
			}
		}
	}
	return f[0], f[1]
}

type freqFile struct{ fd int }

func openFreq(cpu int) freqFile {
	fd, err := unix.Open(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_cur_freq", cpu), unix.O_RDONLY, 0)
	if err != nil {
		return freqFile{-1}
	}
	return freqFile{fd}
}

func (f freqFile) read(buf []byte) int64 {
	if f.fd < 0 {
		return -1
	}
	n, err := unix.Pread(f.fd, buf, 0)
	if err != nil || n <= 0 {
		return -1
	}
	var v int64
	for _, ch := range buf[:n] {
		if ch < '0' || ch > '9' {
			break
		}
		v = v*10 + int64(ch-'0')
	}
	return v
}

func p1Spin(c clk, cpu int, startAbs, endAbs, recNs int64, ready *sync.WaitGroup) p1CPU {
	r := p1CPU{CPU: cpu}
	r.PinErr = pinThis(cpu)
	ss := openSched()
	ff := openFreq(cpu)
	buf := make([]byte, 128)
	r.Gaps = make([]p1Gap, 0, 256)
	ready.Done()
	for c.now() < startAbs {
	}
	baseT := c.now()
	baseRun, baseWait := ss.read(buf)
	last := baseT
	for {
		now := c.now()
		d := now - last
		if d > p1BucketLo {
			r.B100us++
			if d > 1_000_000 {
				r.B1ms++
				if d > 5_000_000 {
					r.B5ms++
					if d > 20_000_000 {
						r.B20ms++
					}
				}
			}
			if d > r.MaxNs {
				r.MaxNs = d
			}
			if d > recNs {
				run, wait := ss.read(buf)
				fq := ff.read(buf)
				g := p1Gap{StartNs: last, DurNs: d, RunNs: -1, WaitNs: -1, OffNs: -1, FreqKHz: fq}
				if run >= 0 && baseRun >= 0 {
					w := now - baseT
					g.RunNs, g.WaitNs = run-baseRun, wait-baseWait
					g.OffNs = max(w-g.RunNs-g.WaitNs, 0)
				}
				if len(r.Gaps) < p1MaxGaps {
					r.Gaps = append(r.Gaps, g)
				} else {
					r.Dropped++
				}
				baseRun, baseWait = ss.read(buf)
				baseT = c.now()
				now = baseT
			}
		}
		r.Reads++
		if now-baseT > 2_000_000 {
			baseRun, baseWait = ss.read(buf)
			baseT = c.now()
			now = baseT
		}
		if now >= endAbs {
			break
		}
		last = now
	}
	return r
}

func p1Sleeper(c clk, cpu int, startAbs, endAbs, recNs int64, ready *sync.WaitGroup) p1CPU {
	r := p1CPU{CPU: cpu}
	r.PinErr = pinThis(cpu)
	ff := openFreq(cpu)
	buf := make([]byte, 128)
	r.Gaps = make([]p1Gap, 0, 64)
	ready.Done()
	for c.now() < startAbs {
		time.Sleep(time.Millisecond)
	}
	ts := unix.Timespec{Sec: 0, Nsec: p1Sleep}
	for {
		t0 := c.now()
		if t0 >= endAbs {
			break
		}
		// The P stays with this thread for a sleep this short (sysmon takes a P
		// back from a syscall only after 10 ms when nothing else wants it), so the
		// elapsed time is the kernel's sleep, not the Go scheduler's.
		_ = unix.ClockNanosleep(unix.CLOCK_MONOTONIC, 0, &ts, nil)
		t1 := c.now()
		over := t1 - t0 - p1Sleep
		r.Reads++
		if over > p1BucketLo {
			r.B100us++
			if over > 1_000_000 {
				r.B1ms++
				if over > 5_000_000 {
					r.B5ms++
					if over > 20_000_000 {
						r.B20ms++
					}
				}
			}
			if over > r.MaxNs {
				r.MaxNs = over
			}
			if over > recNs {
				g := p1Gap{StartNs: t0 + p1Sleep, DurNs: over, RunNs: -1, WaitNs: -1, OffNs: -1, FreqKHz: ff.read(buf)}
				if len(r.Gaps) < p1MaxGaps {
					r.Gaps = append(r.Gaps, g)
				} else {
					r.Dropped++
				}
			}
		}
	}
	return r
}

// ---- sampler ------------------------------------------------------------------

type sampler struct {
	cpus     []int
	freq     []freqFile
	zones    []string
	zoneFD   []int
	psiNames []string
	psiFD    []int
	psiKey   []string // "some" or "full"
	samples  []p1Sample
}

func newSampler(cpus []int) *sampler {
	s := &sampler{cpus: cpus}
	for _, cpu := range cpus {
		s.freq = append(s.freq, openFreq(cpu))
	}
	for i := 0; i < 64; i++ {
		d := fmt.Sprintf("/sys/class/thermal/thermal_zone%d", i)
		typ := readTrim(d + "/type")
		if typ == "" {
			continue
		}
		fd, err := unix.Open(d+"/temp", unix.O_RDONLY, 0)
		if err != nil {
			continue
		}
		s.zones = append(s.zones, fmt.Sprintf("%d:%s", i, typ))
		s.zoneFD = append(s.zoneFD, fd)
	}
	for _, p := range []struct{ res, key string }{{"cpu", "some"}, {"io", "some"}, {"io", "full"}, {"memory", "some"}, {"memory", "full"}} {
		fd, err := unix.Open("/proc/pressure/"+p.res, unix.O_RDONLY, 0)
		s.psiNames = append(s.psiNames, p.res+"."+p.key)
		s.psiFD = append(s.psiFD, map[bool]int{true: fd, false: -1}[err == nil])
		s.psiKey = append(s.psiKey, p.key)
	}
	return s
}

func (s *sampler) sample(c clk) p1Sample {
	sm := p1Sample{T: c.now(), Freq: make([]int64, len(s.cpus)), Temp: make([]int64, len(s.zones)), PSI: make([]uint64, len(s.psiFD))}
	buf := make([]byte, 512)
	for i, f := range s.freq {
		sm.Freq[i] = f.read(buf)
	}
	for i, fd := range s.zoneFD {
		sm.Temp[i] = -1 << 40
		if n, err := unix.Pread(fd, buf, 0); err == nil && n > 0 {
			if v, err := strconv.ParseInt(strings.TrimSpace(string(buf[:n])), 10, 64); err == nil {
				sm.Temp[i] = v
			}
		}
	}
	for i, fd := range s.psiFD {
		if fd < 0 {
			continue
		}
		n, err := unix.Pread(fd, buf, 0)
		if err != nil || n <= 0 {
			continue
		}
		for _, line := range strings.Split(string(buf[:n]), "\n") {
			f := strings.Fields(line)
			if len(f) == 0 || f[0] != s.psiKey[i] {
				continue
			}
			for _, kv := range f[1:] {
				if v, ok := strings.CutPrefix(kv, "total="); ok {
					sm.PSI[i], _ = strconv.ParseUint(v, 10, 64)
				}
			}
		}
	}
	return sm
}

func (s *sampler) run(c clk, from, to int64, stop <-chan struct{}) {
	for c.now() < from {
		select {
		case <-stop:
			return
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	tk := time.NewTicker(p1SampleEvery)
	defer tk.Stop()
	for {
		if len(s.samples) < 3000 {
			s.samples = append(s.samples, s.sample(c))
		}
		select {
		case <-stop:
			return
		case <-tk.C:
		}
		if c.now() > to {
			return
		}
	}
}

// ---- parent -------------------------------------------------------------------

func TestProbeP1TickGap(t *testing.T)  { runP1(t, true) }
func TestProbeP1bIdleGap(t *testing.T) { runP1(t, false) }

func runP1(t *testing.T, spin bool) {
	tp := loadTopo()
	logf(t, "host topology:\n%s", tp.describe())
	cpus := tp.Allowed
	if v := os.Getenv("CELERIS_PROBE_CPUS"); v != "" {
		want, err := parseCPUList(v)
		if err != nil {
			t.Fatal(err)
		}
		var keep []int
		for _, c := range want {
			if _, ok := tp.CPU[c]; ok {
				keep = append(keep, c)
			}
		}
		if len(keep) == 0 {
			t.Fatalf("CELERIS_PROBE_CPUS=%s: none of them is an allowed CPU (%s)", v, fmtCPUs(tp.Allowed))
		}
		cpus = keep
	}
	secs := envInt("CELERIS_PROBE_SECONDS", 60)
	recMs := envInt("CELERIS_PROBE_GAP_MS", 20)
	failMs := envInt("CELERIS_PROBE_FAIL_MS", 500)
	observer := spin && envInt("CELERIS_PROBE_OBSERVER", 1) == 1

	startAbs := nowAbs() + 4*int64(time.Second)
	mk := func(kind string, sampler bool) childSpec {
		env := []string{
			"CELERIS_PROBE_ARG_CPUS=" + fmtCPUs(cpus),
			"CELERIS_PROBE_ARG_START=" + strconv.FormatInt(startAbs, 10),
			"CELERIS_PROBE_ARG_SECS=" + strconv.Itoa(secs),
			"CELERIS_PROBE_ARG_GAP_NS=" + strconv.Itoa(recMs*1_000_000),
			"CELERIS_PROBE_ARG_SAMPLER=" + map[bool]string{true: "1", false: "0"}[sampler],
			"GOGC=off", "GODEBUG=asyncpreemptoff=1", "GOMAXPROCS=" + strconv.Itoa(len(cpus)+2),
		}
		return childSpec{kind: kind, env: env, timeout: time.Duration(secs+90) * time.Second}
	}
	type out struct {
		res    p1Result
		stderr []byte
		err    error
	}
	var specs []childSpec
	if spin {
		specs = append(specs, mk("p1spin", true))
		if observer {
			specs = append(specs, mk("p1sleep", false))
		}
	} else {
		specs = append(specs, mk("p1sleep", true))
	}
	outs := make([]out, len(specs))
	var wg sync.WaitGroup
	for i, sp := range specs {
		wg.Go(func() {
			so, se, err := runChild(sp)
			outs[i].stderr = se
			if err != nil {
				outs[i].err = fmt.Errorf("%s: %v\n%s", sp.kind, err, tailLines(se, 20))
				return
			}
			if err := decodeChild(so, &outs[i].res); err != nil {
				outs[i].err = fmt.Errorf("%s: %v\n%s", sp.kind, err, tailLines(se, 20))
			}
		})
	}
	wg.Wait()
	var procs []p1Result
	for _, o := range outs {
		if o.err != nil {
			t.Fatalf("a measuring child failed: %v", o.err)
		}
		procs = append(procs, o.res)
	}
	rep := analyzeP1(tp, procs, startAbs, int64(recMs)*1_000_000)
	logf(t, "%s", rep.text)
	if rep.maxGapNs > int64(failMs)*1_000_000 {
		t.Errorf("a gap of %s ms (above the %d ms limit) on %s: see the table above", ms(rep.maxGapNs), failMs, rep.maxWhere)
	}
}

type p1Report struct {
	text     string
	maxGapNs int64
	maxWhere string
}

type p1Hit struct {
	proc int
	cpu  int
	g    p1Gap
}

func (h p1Hit) end() int64 { return h.g.StartNs + h.g.DurNs }

func gapKind(g p1Gap) string {
	if g.RunNs < 0 || g.WaitNs < 0 {
		return "?"
	}
	half := g.DurNs / 2
	switch {
	case g.WaitNs >= half:
		return "preempted"
	case g.OffNs >= half:
		return "blocked"
	}
	return "on-cpu"
}

func analyzeP1(tp topo, procs []p1Result, startAbs, recNs int64) p1Report {
	var b strings.Builder
	var rep p1Report
	name := func(p int) string {
		if procs[p].Kind == "p1spin" {
			return "spin"
		}
		return "sleep"
	}
	var hits []p1Hit
	total := make([]int, len(procs))
	for pi, p := range procs {
		total[pi] = len(p.CPUs)
		for _, c := range p.CPUs {
			for _, g := range c.Gaps {
				hits = append(hits, p1Hit{pi, c.CPU, g})
			}
			if c.MaxNs > rep.maxGapNs {
				rep.maxGapNs = c.MaxNs
				rep.maxWhere = fmt.Sprintf("%s process cpu %d", name(pi), c.CPU)
			}
		}
	}
	fmt.Fprintf(&b, "P1 window: %d s from t0 (CLOCK_MONOTONIC %d), record threshold %s ms\n", (procs[0].EndNs-procs[0].StartNs)/1e9, startAbs, ms(recNs))
	for pi, p := range procs {
		fmt.Fprintf(&b, "process %s: pid %d, GOMAXPROCS %d, NumCPU %d, %d CPUs observed", name(pi), p.Pid, p.GoMaxProc, p.NumCPU, len(p.CPUs))
		for _, c := range p.CPUs {
			if c.PinErr != "" {
				fmt.Fprintf(&b, "; cpu %d pin error: %s", c.CPU, c.PinErr)
			}
		}
		b.WriteString("\n")
	}

	// per-CPU summary
	for pi, p := range procs {
		fmt.Fprintf(&b, "\nper-CPU, %s process (reads or sleeps; gaps by size)\n", name(pi))
		fmt.Fprintf(&b, "%-4s %-6s %-5s %-7s %-7s %12s %8s %7s %7s %7s %10s %s\n", "cpu", "class", "cap", "maxMHz", "domain", "reads", ">100us", ">1ms", ">5ms", ">20ms", "max_ms", "recorded")
		for _, c := range p.CPUs {
			ci := tp.CPU[c.CPU]
			fmt.Fprintf(&b, "%-4d %-6s %-5d %-7s %-7s %12d %8d %7d %7d %7d %10s %d", c.CPU, ci.class(), ci.Cap, mhz(int64(ci.MaxKHz)), orDash(ci.Domain), c.Reads, c.B100us, c.B1ms, c.B5ms, c.B20ms, ms(c.MaxNs), len(c.Gaps))
			if c.Dropped > 0 {
				fmt.Fprintf(&b, " (+%d dropped)", c.Dropped)
			}
			b.WriteString("\n")
		}
	}

	// events: gaps that overlap in time (5 ms slack) across CPUs and processes
	sort.Slice(hits, func(i, j int) bool { return hits[i].g.StartNs < hits[j].g.StartNs })
	type event struct {
		hits  []p1Hit
		s, e  int64
		maxNs int64
	}
	var evs []event
	for _, h := range hits {
		if n := len(evs); n > 0 && h.g.StartNs <= evs[n-1].e+5_000_000 {
			ev := &evs[n-1]
			ev.hits = append(ev.hits, h)
			ev.e = max(ev.e, h.end())
			ev.maxNs = max(ev.maxNs, h.g.DurNs)
			continue
		}
		evs = append(evs, event{hits: []p1Hit{h}, s: h.g.StartNs, e: h.end(), maxNs: h.g.DurNs})
	}
	var sam *p1Result
	for i := range procs {
		if len(procs[i].Samples) > 0 {
			sam = &procs[i]
		}
	}
	fmt.Fprintf(&b, "\nEVENTS: gaps above %s ms that overlap in time, merged (%d gaps in %d events)\n", ms(recNs), len(hits), len(evs))
	if len(evs) == 0 {
		b.WriteString("none: no gap above the threshold on any observed CPU\n")
	} else {
		fmt.Fprintf(&b, "%-4s %9s %9s %-22s %-8s %-10s %-34s %s\n", "ev", "t+s", "max_ms", "CPUs hit per process", "scope", "kinds", "freq before>after (hit CPUs, MHz)", "temp_max_C psi_delta_us(cpu/io/mem some)")
		for i, ev := range evs {
			hit := make([]map[int]bool, len(procs))
			for k := range hit {
				hit[k] = map[int]bool{}
			}
			kinds := map[string]int{}
			for _, h := range ev.hits {
				hit[h.proc][h.cpu] = true
				if procs[h.proc].Kind == "p1spin" {
					kinds[gapKind(h.g)]++
				}
			}
			var per []string
			for k := range procs {
				per = append(per, fmt.Sprintf("%s %d/%d", name(k), len(hit[k]), total[k]))
			}
			scope := p1Scope(procs, hit, total)
			var ks []string
			for k, n := range kinds {
				ks = append(ks, fmt.Sprintf("%s:%d", k, n))
			}
			sort.Strings(ks)
			fq, tmp, psi := "-", "-", "-"
			if sam != nil {
				fq, tmp, psi = p1Context(sam, ev.s, ev.e, hit)
			}
			fmt.Fprintf(&b, "%-4d %9.3f %9s %-22s %-8s %-10s %-34s %s %s\n", i+1, float64(ev.s-startAbs)/1e9, ms(ev.maxNs), strings.Join(per, " "), scope, strings.Join(ks, ","), fq, tmp, psi)
		}
	}
	// the longest gaps one by one
	sort.Slice(hits, func(i, j int) bool { return hits[i].g.DurNs > hits[j].g.DurNs })
	if len(hits) > 0 {
		fmt.Fprintf(&b, "\nLONGEST GAPS (up to 25)\n%-6s %-4s %-6s %9s %9s %9s %9s %9s %9s %s\n", "proc", "cpu", "class", "t+s", "gap_ms", "run_ms", "wait_ms", "off_ms", "kind", "freq_MHz_after")
		for i, h := range hits {
			if i >= 25 {
				break
			}
			g := h.g
			fmt.Fprintf(&b, "%-6s %-4d %-6s %9.3f %9s %9s %9s %9s %9s %s\n", name(h.proc), h.cpu, tp.CPU[h.cpu].class(), float64(g.StartNs-startAbs)/1e9, ms(g.DurNs), msN(g.RunNs), msN(g.WaitNs), msN(g.OffNs), gapKind(g), mhz(g.FreqKHz))
		}
	}
	if sam != nil {
		b.WriteString("\n")
		b.WriteString(p1Samples(sam))
	}
	fmt.Fprintf(&b, "\nverdict input: longest single gap %s ms (%s)", ms(rep.maxGapNs), orDash(rep.maxWhere))
	rep.text = b.String()
	return rep
}

func mhz(khz int64) string {
	if khz <= 0 {
		return "n/a"
	}
	return strconv.FormatInt(khz/1000, 10)
}

func msN(ns int64) string {
	if ns < 0 {
		return "?"
	}
	return ms(ns)
}

func distinctCPUs(hit []map[int]bool) int {
	all := map[int]bool{}
	for _, m := range hit {
		for c := range m {
			all[c] = true
		}
	}
	return len(all)
}

func p1Scope(procs []p1Result, hit []map[int]bool, total []int) string {
	frac := make([]float64, len(procs))
	for k := range procs {
		frac[k] = float64(len(hit[k])) / float64(max(total[k], 1))
	}
	switch {
	case len(procs) == 2 && frac[0] >= 0.9 && frac[1] >= 0.9:
		return "MACHINE"
	case len(procs) == 2 && frac[0] >= 0.9 && frac[1] <= 0.25:
		return "PROCESS"
	case len(procs) == 2 && frac[1] >= 0.9 && frac[0] <= 0.25:
		return "PROCESS-B"
	case len(procs) == 1 && frac[0] >= 0.9:
		return "ALL-CPUS"
	case distinctCPUs(hit) <= 1:
		return "ONE-CPU"
	}
	return "PARTIAL"
}

// p1Context returns the frequency of the hit CPUs in the samples just before
// and just after the event, the hottest thermal zone, and the pressure totals'
// growth (cpu, io, memory "some") across the event.
func p1Context(r *p1Result, s, e int64, hit []map[int]bool) (fq, tmp, psi string) {
	var before, after *p1Sample
	for i := range r.Samples {
		sm := &r.Samples[i]
		if sm.T <= s {
			before = sm
		}
		if sm.T >= e && after == nil {
			after = sm
		}
	}
	if before == nil || after == nil {
		return "-", "-", "-"
	}
	// the sampler lists the CPUs in r.CPUs order
	var parts []string
	shown := 0
	for i, c := range r.CPUs {
		inHit := false
		for _, m := range hit {
			if m[c.CPU] {
				inHit = true
			}
		}
		if !inHit || shown >= 4 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d:%s>%s", c.CPU, mhz(before.Freq[i]), mhz(after.Freq[i])))
		shown++
	}
	fq = strings.Join(parts, " ")
	if fq == "" {
		fq = "-"
	}
	var hot int64 = -1 << 40
	for _, v := range after.Temp {
		hot = max(hot, v)
	}
	for _, v := range before.Temp {
		hot = max(hot, v)
	}
	if hot > -1<<39 {
		tmp = fmt.Sprintf("%.1f", float64(hot)/1000)
	} else {
		tmp = "-"
	}
	d := func(i int) string {
		if i >= len(after.PSI) {
			return "-"
		}
		return strconv.FormatUint(after.PSI[i]-before.PSI[i], 10)
	}
	psi = d(0) + "/" + d(1) + "/" + d(3)
	return fq, tmp, psi
}

// p1Samples prints the sampler's series compactly: the extremes over the
// window, so a throttled or hot run is visible even without a gap.
func p1Samples(r *p1Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SAMPLER (every %d ms, %d samples)\n", p1SampleEvery/time.Millisecond, len(r.Samples))
	if len(r.Samples) == 0 {
		return b.String()
	}
	fmt.Fprintf(&b, "%-4s %12s %12s\n", "cpu", "minMHz", "maxMHz")
	for i, c := range r.CPUs {
		lo, hi := int64(1<<62), int64(-1)
		for _, s := range r.Samples {
			if v := s.Freq[i]; v >= 0 {
				lo, hi = min(lo, v), max(hi, v)
			}
		}
		if hi < 0 {
			fmt.Fprintf(&b, "%-4d %12s %12s\n", c.CPU, "n/a", "n/a")
			continue
		}
		fmt.Fprintf(&b, "%-4d %12d %12d\n", c.CPU, lo/1000, hi/1000)
	}
	if len(r.Zones) > 0 {
		fmt.Fprintf(&b, "thermal zones: min/max C over the window\n")
		for i, z := range r.Zones {
			lo, hi := int64(1<<62), int64(-1<<62)
			for _, s := range r.Samples {
				if v := s.Temp[i]; v > -1<<39 {
					lo, hi = min(lo, v), max(hi, v)
				}
			}
			fmt.Fprintf(&b, "  zone %-24s %.1f / %.1f\n", z, float64(lo)/1000, float64(hi)/1000)
		}
	} else {
		b.WriteString("thermal zones: none readable\n")
	}
	first, last := r.Samples[0], r.Samples[len(r.Samples)-1]
	fmt.Fprintf(&b, "pressure totals grown over the window (us):")
	for i, n := range r.PSINames {
		if i < len(first.PSI) {
			fmt.Fprintf(&b, " %s=%d", n, last.PSI[i]-first.PSI[i])
		}
	}
	b.WriteString("\n")
	return b.String()
}
