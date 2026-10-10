//go:build linux

package stallprobe

// E0: what a SIGURG costs on each CPU class.
//
//	go test -run '^TestProbeE0SigCost$'      (celeris-stress: -f run='^TestProbeE0SigCost$')
//
// Per allowed CPU (a locked thread pinned to it, in a child process):
//
//	null    raw getppid(2): the syscall entry+exit cost
//	self    raw tgkill(own tid, SIGURG), including Go's SIGURG handler (doSigPreempt,
//	        asyncpreemptoff=0) and rt_sigreturn: the cycle time of a thread that is
//	        sent a signal the moment its previous one returns ("persistence": while the
//	        sender keeps re-sending, every cycle must be >= ~5 us for suspendG's resend
//	        to survive, and a target never runs user code between two cycles when the next
//	        signal is already pending at sigreturn)
//	wake    one idle epoll_wait(1 ms): overshoot beyond 1 ms, and thread CPU time per wake
//
// Cross CPU ("entry": can a storm start?). A target thread pinned to CPU t spins
// in user code reading CLOCK_MONOTONIC; a signal shows up as a gap between two
// reads. A sender pinned to CPU s records t_send, tgkills the target, and polls
// until the target publishes a gap that began after t_send. With
//
//	gs, ge   the gap's first and last timestamps (the last read before the
//	         interruption, the first read after rt_sigreturn)
//
// the send-to-acknowledge latency L (t_send to preemptGen++, which is inside the
// handler) satisfies
//
//	lo = gs - t_send  <=  L  <=  hi = ge - t_send
//
// because the acknowledge is after the kernel interrupted the target and before
// it returned. hi overstates L by the handler's tail and the sigreturn, and both
// bounds include the sender's syscall entry (t_send is read before the syscall).
// The gap length (ge - gs) is the width of the uncertainty and is printed. A
// storm needs L >= 5 us (suspendG re-sends only if the acknowledge was not seen
// within yieldDelay/2): P(hi < 5 us) is the share of signals that are surely
// dropped, P(lo >= 5 us) the share that surely cross the limit; the rest is
// ambiguous. Why not SigPnd or a procfs poll: one read costs >= 10 us, far above
// the 5 us threshold. Why not SIGUSR1/os/signal: the Go handler of SIGURG is the
// code under test.
//
// Error sources: tick interrupts and sysmon's 10 ms preemption can land in a
// sample (they are screened when they begin before t_send, and show in the p99+
// columns, not in the medians); the clock read costs ~30-60 ns (the loop's
// median iteration is printed as the resolution); a gap shorter than
// max(500 ns, 10 x the median iteration) is not seen (the signal path is longer).
// The test passes unless a child fails or no sample was taken at all.

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type e0Self struct {
	CPU      int
	Class    string
	Gov      string
	KHz0     int
	KHz1     int
	NullNs   float64
	NullN    int
	SelfNs   float64
	SelfN    int
	WakeOver float64 // ns beyond 1 ms, mean
	WakeMax  int64
	WakeCPU  float64 // thread CPU ns per wake
	WakeN    int
	Err      string
}

type e0Pair struct {
	Sender, Target   int
	SClass, TClass   string
	N, Miss          int
	Hi, Lo, Gap, Snd pcts
	HiLT5, LoGE5     int
	LoopMedNs        int64
	ThrNs            int64
	TickNs           int64 // smallest nonzero step of the clock
}

type e0Group struct {
	SClass, TClass string
	Pairs          int
	N, Miss        int
	Hi, Lo, Gap    pcts
	HiLT5, LoGE5   int
}

type e0Result struct {
	Facts   string
	Vulns   []string
	Dmesg   []string
	Self    []e0Self
	Pairs   []e0Pair
	Groups  []e0Group
	Budget  bool
	Elapsed float64
}

func e0Child() (any, error) {
	start := time.Now()
	tp := loadTopo()
	c := newClk()
	res := &e0Result{Facts: hostFacts()}
	res.Vulns = vulnerabilities()
	res.Dmesg = dmesgHints()
	selfN := envInt("CELERIS_PROBE_E0_SELF_N", 100000)
	samples := envInt("CELERIS_PROBE_E0_SAMPLES", 10000)
	budget := time.Duration(envInt("CELERIS_PROBE_E0_BUDGET_S", 240)) * time.Second

	for _, cpu := range tp.Allowed {
		out := make(chan e0Self, 1)
		go func() { out <- e0SelfOne(c, tp, cpu, selfN) }()
		res.Self = append(res.Self, <-out)
	}

	// Senders: the fastest and the slowest CPU of every class (or all).
	var senders []int
	if os.Getenv("CELERIS_PROBE_E0_SENDERS") == "all" {
		senders = sortedCopy(tp.Allowed)
	} else {
		for _, g := range tp.Classes {
			o := tp.byFastness(g)
			senders = append(senders, o[0])
			if len(o) > 1 {
				senders = append(senders, o[len(o)-1])
			}
		}
	}
	groups := map[string]*e0Group{}
	raw := map[string]*[3][]int64{} // pooled hi, lo, gap per group
	var order []string
loop:
	for _, s := range senders {
		for _, t := range tp.Allowed {
			if s == t {
				continue
			}
			if time.Since(start) > budget {
				res.Budget = true
				break loop
			}
			out := make(chan e0Pair, 1)
			var hi, lo, gp []int64
			go func() {
				p, h, l, g := e0PairRun(c, tp, s, t, samples)
				hi, lo, gp = h, l, g
				out <- p
			}()
			p := <-out
			res.Pairs = append(res.Pairs, p)
			key := p.SClass + ">" + p.TClass
			g := groups[key]
			if g == nil {
				g = &e0Group{SClass: p.SClass, TClass: p.TClass}
				groups[key] = g
				raw[key] = &[3][]int64{}
				order = append(order, key)
			}
			g.Pairs++
			g.N += p.N
			g.Miss += p.Miss
			g.HiLT5 += p.HiLT5
			g.LoGE5 += p.LoGE5
			r := raw[key]
			r[0], r[1], r[2] = append(r[0], hi...), append(r[1], lo...), append(r[2], gp...)
		}
	}
	for _, k := range order {
		g := groups[k]
		g.Hi, g.Lo, g.Gap = percentiles(raw[k][0]), percentiles(raw[k][1]), percentiles(raw[k][2])
		res.Groups = append(res.Groups, *g)
	}
	res.Elapsed = time.Since(start).Seconds()
	return res, nil
}

func cpuKHz(cpu int) int {
	return readInt(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_cur_freq", cpu), -1)
}

// e0SelfOne measures one CPU. It runs on a goroutine of its own, locked and
// never unlocked, so the pinned thread ends with the goroutine.
func e0SelfOne(c clk, tp topo, cpu, n int) e0Self {
	r := e0Self{CPU: cpu, Class: classLabel(tp, cpu), KHz0: cpuKHz(cpu)}
	r.Gov = readTrim(fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_governor", cpu))
	if e := pinThis(cpu); e != "" {
		r.Err = e
		return r
	}
	pid, tid := unix.Getpid(), unix.Gettid()
	for range 5000 { // warm: handler pages, caches, frequency
		rawTgkill(pid, tid, unix.SIGURG)
	}
	// null syscall
	t0 := c.now()
	capNs := int64(1_000_000_000)
	done := 0
	for done < 20*n {
		for range 256 {
			rawNullSyscall()
		}
		done += 256
		if c.now()-t0 > capNs {
			break
		}
	}
	r.NullN, r.NullNs = done, float64(c.now()-t0)/float64(done)
	// self signal
	t0 = c.now()
	capNs = 2_000_000_000
	done = 0
	for done < n {
		for range 64 {
			rawTgkill(pid, tid, unix.SIGURG)
		}
		done += 64
		if c.now()-t0 > capNs {
			break
		}
	}
	r.SelfN, r.SelfNs = done, float64(c.now()-t0)/float64(done)
	// idle 1 ms epoll wake
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err == nil {
		evs := make([]unix.EpollEvent, 4)
		var ts0, ts1 unix.Timespec
		_ = unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts0)
		var over float64
		const wakes = 200
		nw := 0
		for range wakes {
			a := c.now()
			if _, e := unix.EpollWait(epfd, evs, 1); e != nil {
				continue
			}
			nw++
			o := c.now() - a - 1_000_000
			over += float64(o)
			if o > r.WakeMax {
				r.WakeMax = o
			}
		}
		_ = unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &ts1)
		_ = unix.Close(epfd)
		if nw > 0 {
			r.WakeN, r.WakeOver, r.WakeCPU = nw, over/float64(nw), float64(ts1.Nano()-ts0.Nano())/float64(nw)
		}
	}
	r.KHz1 = cpuKHz(cpu)
	return r
}

// e0Target is the spinning target of a cross-CPU measurement.
type e0Target struct {
	tid    atomic.Int64
	ready  atomic.Bool
	stop   atomic.Bool
	seq    atomic.Int64
	gs, ge atomic.Int64
	tickNs atomic.Int64
	medNs  atomic.Int64
	thrNs  atomic.Int64
	pinErr atomic.Value
	exited atomic.Bool
}

func (tg *e0Target) run(c clk, cpu int) {
	defer tg.exited.Store(true)
	if e := pinThis(cpu); e != "" {
		tg.pinErr.Store(e)
		tg.ready.Store(true)
		return
	}
	tg.tid.Store(int64(unix.Gettid()))
	// calibrate the loop: median iteration over 8192 reads
	var d [8192]int64
	last := c.now()
	for i := range d {
		now := c.now()
		d[i] = now - last
		last = now
	}
	s := d[:]
	slices.Sort(s)
	med := s[len(s)/2]
	thr := max(500, 10*med)
	tick := int64(0)
	for _, x := range s {
		if x > 0 {
			tick = x
			break
		}
	}
	tg.tickNs.Store(tick)
	tg.medNs.Store(med)
	tg.thrNs.Store(thr)
	tg.ready.Store(true)
	last = c.now()
	for !tg.stop.Load() {
		now := c.now()
		if now-last > thr {
			tg.gs.Store(last)
			tg.ge.Store(now)
			tg.seq.Add(1)
		}
		last = now
	}
}

func e0PairRun(c clk, tp topo, sender, target, samples int) (e0Pair, []int64, []int64, []int64) {
	p := e0Pair{Sender: sender, Target: target, SClass: classLabel(tp, sender), TClass: classLabel(tp, target)}
	pid := unix.Getpid()
	tg := &e0Target{}
	go tg.run(c, target)
	if e := pinThis(sender); e != "" {
		tg.stop.Store(true)
		return p, nil, nil, nil
	}
	for dl := time.Now().Add(10 * time.Second); !tg.ready.Load() && time.Now().Before(dl); {
		time.Sleep(time.Millisecond)
	}
	if e, _ := tg.pinErr.Load().(string); e != "" || !tg.ready.Load() {
		tg.stop.Store(true)
		return p, nil, nil, nil
	}
	tid := int(tg.tid.Load())
	p.LoopMedNs, p.ThrNs, p.TickNs = tg.medNs.Load(), tg.thrNs.Load(), tg.tickNs.Load()
	his, los, gaps, snds := make([]int64, 0, samples), make([]int64, 0, samples), make([]int64, 0, samples), make([]int64, 0, samples)
	rng := uint64(0x9E3779B97F4A7C15) ^ uint64(sender*131+target)
	next := func() uint64 { rng ^= rng << 13; rng ^= rng >> 7; rng ^= rng << 17; return rng }
	for i := 0; i < samples; i++ {
		// 20-50 us apart, jittered, with the sender spinning (hot, like suspendG)
		spinFor(c, 20_000+int64(next()%30_000))
		seq0 := tg.seq.Load()
		ts := c.now()
		rawTgkill(pid, tid, unix.SIGURG)
		tr := c.now()
		hit := false
		for {
			now := c.now()
			if now-ts > 2_000_000 {
				break
			}
			s := tg.seq.Load()
			if s == seq0 {
				continue
			}
			gs, ge := tg.gs.Load(), tg.ge.Load()
			if ge < gs || gs < ts-100 { // torn read, or a gap that began before the send (a tick)
				seq0 = s
				continue
			}
			if tg.seq.Load() != s {
				continue
			}
			his = append(his, ge-ts)
			los = append(los, gs-ts)
			gaps = append(gaps, ge-gs)
			snds = append(snds, tr-ts)
			hit = true
			break
		}
		p.N++
		if !hit {
			p.Miss++
		}
	}
	tg.stop.Store(true)
	for dl := time.Now().Add(2 * time.Second); !tg.exited.Load() && time.Now().Before(dl); {
		time.Sleep(time.Millisecond)
	}
	p.Hi, p.Lo, p.Gap, p.Snd = percentiles(his), percentiles(los), percentiles(gaps), percentiles(snds)
	for i := range his {
		if his[i] < 5000 {
			p.HiLT5++
		}
		if los[i] >= 5000 {
			p.LoGE5++
		}
	}
	return p, his, los, gaps
}

func vulnerabilities() []string {
	var out []string
	ents, err := os.ReadDir("/sys/devices/system/cpu/vulnerabilities")
	if err != nil {
		return nil
	}
	for _, e := range ents {
		out = append(out, e.Name()+": "+readTrim("/sys/devices/system/cpu/vulnerabilities/"+e.Name()))
	}
	return out
}

// dmesgHints reads kernel lines about mitigations and workarounds, if the host
// lets this user read the ring buffer; a refusal is not an error.
func dmesgHints() []string {
	cmd := exec.Command("dmesg")
	done := make(chan []byte, 1)
	go func() { b, _ := cmd.Output(); done <- b }()
	var b []byte
	select {
	case b = <-done:
	case <-time.After(5 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		ll := strings.ToLower(l)
		if strings.Contains(ll, "spectre") || strings.Contains(ll, "bhb") || strings.Contains(ll, "erratum") ||
			strings.Contains(ll, "errata") || strings.Contains(ll, "workaround") || strings.Contains(ll, "mitigation") {
			if len(out) < 25 {
				out = append(out, strings.TrimSpace(l))
			}
		}
	}
	return out
}

func pct(a, b int) string {
	if b == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(a)/float64(b))
}

func TestProbeE0SigCost(t *testing.T) {
	tp := loadTopo()
	logf(t, "E0 per-CPU-class signal cost\nhost: %s\ntopology:\n%s", hostFacts(), tp.describe())
	samples := envInt("CELERIS_PROBE_E0_SAMPLES", 10000)
	budget := envInt("CELERIS_PROBE_E0_BUDGET_S", 240)
	so, se, err := runChild(childSpec{kind: "e0", env: []string{"GODEBUG=asyncpreemptoff=0"}, timeout: time.Duration(budget+240) * time.Second})
	if err != nil {
		t.Fatalf("the child failed: %v\n%s", err, tailLines(se, 20))
	}
	var res e0Result
	if err := decodeChild(so, &res); err != nil {
		t.Fatalf("%v\n%s", err, tailLines(se, 20))
	}
	logf(t, "child: %s; %.0f s%s", res.Facts, res.Elapsed, map[bool]string{true: "; THE BUDGET STOPPED THE PAIRS EARLY", false: ""}[res.Budget])
	if len(res.Vulns) > 0 {
		logf(t, "cpu vulnerabilities (sysfs): %s", strings.Join(res.Vulns, " | "))
	}
	if len(res.Dmesg) > 0 {
		logf(t, "dmesg hints:\n  %s", strings.Join(res.Dmesg, "\n  "))
	} else {
		logf(t, "dmesg hints: none readable or none present")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "SELF per CPU: null syscall, self tgkill(SIGURG) round trip incl. Go handler + sigreturn, 1 ms epoll wake (us)\n")
	fmt.Fprintf(&b, "  cpu  class   governor   MHz(before>after)  null_us  self_us  self-null_us  self_n   wake_over_us  wake_max_us  wake_cpu_us")
	for _, s := range res.Self {
		if s.Err != "" {
			fmt.Fprintf(&b, "\n  %-4d %-7s ERROR %s", s.CPU, s.Class, s.Err)
			continue
		}
		fmt.Fprintf(&b, "\n  %-4d %-7s %-10s %-18s %-8s %-8s %-13s %-8d %-13s %-12s %s",
			s.CPU, s.Class, orDash(s.Gov), fmt.Sprintf("%s>%s", mhz(int64(s.KHz0)), mhz(int64(s.KHz1))),
			fmt.Sprintf("%.3f", s.NullNs/1e3), usF(s.SelfNs), usF(s.SelfNs-s.NullNs), s.SelfN,
			usF(s.WakeOver), us(s.WakeMax), usF(s.WakeCPU))
	}
	logf(t, "%s", b.String())

	// class medians of the self round trip
	selfBy := map[string][]float64{}
	var classes []string
	for _, s := range res.Self {
		if s.Err == "" {
			if _, ok := selfBy[s.Class]; !ok {
				classes = append(classes, s.Class)
			}
			selfBy[s.Class] = append(selfBy[s.Class], s.SelfNs)
		}
	}
	var sl []string
	for _, k := range classes {
		v := selfBy[k]
		slices.Sort(v)
		sl = append(sl, fmt.Sprintf("%s median %s us (min %s, max %s, %d CPUs)", k, usF(v[len(v)/2]), usF(v[0]), usF(v[len(v)-1]), len(v)))
	}
	logf(t, "SELF round trip by class: %s   [threshold: 5 us]", strings.Join(sl, "; "))

	b.Reset()
	fmt.Fprintf(&b, "CROSS-CPU send-to-acknowledge, per pair (us). lo <= L <= hi; gap = hi-lo + handler tail; P5 columns: share of signals with hi < 5 us (surely dropped by suspendG) / lo >= 5 us (surely resent)\n")
	fmt.Fprintf(&b, "  s>t   classes         n     miss   hi_p50 hi_p90 hi_p99   lo_p50 lo_p99   gap_p50  send_p50  clk_tick/med(thr)_ns   hi<5us   lo>=5us")
	for _, p := range res.Pairs {
		got := p.N - p.Miss
		fmt.Fprintf(&b, "\n  %-2d>%-3d %-14s %-5d %-6d %-6s %-6s %-6s   %-6s %-6s   %-7s  %-8s  %-18s  %-8s %s",
			p.Sender, p.Target, p.SClass+">"+p.TClass, p.N, p.Miss, us(p.Hi.P50), us(p.Hi.P90), us(p.Hi.P99), us(p.Lo.P50), us(p.Lo.P99), us(p.Gap.P50), us(p.Snd.P50),
			fmt.Sprintf("%d/%d(%d)", p.TickNs, p.LoopMedNs, p.ThrNs), pct(p.HiLT5, got), pct(p.LoGE5, got))
	}
	logf(t, "%s", b.String())

	b.Reset()
	fmt.Fprintf(&b, "CROSS-CPU by class pair, pooled over %d pairs (us)\n", len(res.Pairs))
	fmt.Fprintf(&b, "  sender>target   pairs  samples  miss    hi_p50 hi_p90 hi_p99 hi_max   lo_p50 lo_p90 lo_p99   gap_p50  P(hi<5us)  P(lo>=5us)")
	taken := 0
	for _, g := range res.Groups {
		got := g.N - g.Miss
		taken += got
		fmt.Fprintf(&b, "\n  %-15s %-6d %-8d %-7s %-6s %-6s %-6s %-7s  %-6s %-6s %-6s   %-7s  %-10s %s",
			g.SClass+">"+g.TClass, g.Pairs, g.N, pct(g.Miss, g.N), us(g.Hi.P50), us(g.Hi.P90), us(g.Hi.P99), us(g.Hi.Max), us(g.Lo.P50), us(g.Lo.P90), us(g.Lo.P99), us(g.Gap.P50), pct(g.HiLT5, got), pct(g.LoGE5, got))
	}
	logf(t, "%s", b.String())
	if taken == 0 {
		t.Fatalf("no cross-CPU sample was taken (%d pairs): the method did not work on this host", len(res.Pairs))
	}
	var sum []string
	for _, g := range res.Groups {
		got := g.N - g.Miss
		sum = append(sum, fmt.Sprintf("%s hi_p50 %s lo_p50 %s us P(hi<5)=%s P(lo>=5)=%s", g.SClass+">"+g.TClass, us(g.Hi.P50), us(g.Lo.P50), pct(g.HiLT5, got), pct(g.LoGE5, got)))
	}
	logf(t, "E0 SUMMARY %s (samples/pair %d): self %s | cross %s", runtime.GOARCH, samples, strings.Join(sl, "; "), strings.Join(sum, "; "))
}
