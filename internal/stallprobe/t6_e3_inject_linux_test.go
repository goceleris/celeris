//go:build linux

package stallprobe

// E3: a SIGURG injector with no GC and no allocation.
//
//	go test -run '^TestProbeE3SigInject$'   (celeris-stress: -f run='^TestProbeE3SigInject$')
//
// One child process per CPU class (fastest class first). In it, K locked, pinned
// loop threads (default 4 of the class), each in unix.EpollWait(idle epfd, 1 ms)
// followed by a little user work per timeout wake-up, and ONE sender thread pinned
// to the fastest CPU of the fastest class that is not a loop CPU. The sender
// tgkills SIGURG at a fixed rate r per second to ONE of the loops (the target) for
// a few seconds, for r in {0 (baseline), 1k, 10k, 50k, 100k, 200k}; the other loops
// of the class are bystanders that get no signal (a control for host-wide
// effects). 5 us per signal is 200k/s: the rate at which suspendG's 5 us resend
// limit would be exceeded by a target that needs 5 us per signal.
//
// Per class and rate the table prints, for the target and (median of) the
// bystanders: returns/s of epoll_wait (ret), timeouts/s (tmo), EINTRs/s, the
// longest gap between two returns (gap_any) and between two timeouts (gap_tmo),
// the share of periodic samples in which the thread was in state R (not
// sleeping), user and system CPU of the thread, and involuntary context
// switches. Like celeris, the loop restarts the wait on EINTR, so each signal
// restarts the 1 ms timeout: above ~1k signals/s the timeout count falls to
// zero on every CPU class, by construction. The liveness numbers are therefore
// ret/s and gap_any (the thread is running at all).
//
// r* (printed per class, both rules are heuristics, the raw table is the evidence):
//
//	r*stall      the lowest r with target gap_any >= 100 ms or target ret/s < 50% of
//	             the r=0 ret/s: the thread stopped running the loop
//	r*coalesce   the lowest r with target ret/s < 50% of the achieved sent/s: signals
//	             arrive faster than the target absorbs them (a standard signal that is
//	             already pending is dropped). The target here is mostly SLEEPING in
//	             epoll_wait, so 1/(ret/s) is the cost of a whole wake-handler-return-
//	             re-enter cycle, not the latency to acknowledge a signal that hits a
//	             running thread (E0 measures that, against suspendG's 5 us)
//
// The sender is a plain spinning thread; its achieved rate is printed (a tgkill
// slower than the interval lowers it, it never bursts to catch up). The test
// fails only if a child fails.

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type e3Loop struct {
	CPU                  int
	Role                 string
	Wakes, Eintr, OthErr uint64
	GapAnyNs, GapWakeNs  int64
	Gap50                uint64
	RSamples, Samples    int
	Ut, St, Vol, Invol   int64 // clock ticks / counts over the phase
}

type e3Phase struct {
	Rate     int
	Sent     int64
	Secs     float64
	SentPerS float64
	Loops    []e3Loop
}

type e3Result struct {
	Facts  string
	Class  string
	Sender int
	Target int
	CPUs   []int
	Phases []e3Phase
}

func e3Child() (any, error) {
	c := newClk()
	cpus, err := parseCPUList(os.Getenv("CELERIS_PROBE_ARG_E3_LOOPS"))
	if err != nil || len(cpus) == 0 {
		return nil, fmt.Errorf("CELERIS_PROBE_ARG_E3_LOOPS: %v", err)
	}
	sender := envInt("CELERIS_PROBE_ARG_E3_SENDER", -1)
	if sender < 0 {
		return nil, fmt.Errorf("no sender cpu")
	}
	var rates []int
	for _, s := range envList("CELERIS_PROBE_E3_RATES", "1000,10000,50000,100000,200000") {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("CELERIS_PROBE_E3_RATES: %q", s)
		}
		rates = append(rates, n)
	}
	rates = append([]int{0}, rates...)
	dur := time.Duration(envInt("CELERIS_PROBE_E3_SECS", 3)) * time.Second
	workNs := int64(envInt("CELERIS_PROBE_E3_WORK_US", 20)) * 1000
	res := &e3Result{Facts: hostFacts(), Sender: sender, Target: cpus[0], CPUs: cpus}

	var stop atomic.Bool
	loops, err := startLoops(c, cpus, workNs, 0, &stop)
	if err != nil {
		stop.Store(true)
		return nil, err
	}
	defer stop.Store(true)
	pid := os.Getpid()
	tid := int(loops[0].Tid.Load())

	type cmd struct {
		rate int
		dur  time.Duration
		done chan int64
	}
	cmds := make(chan cmd)
	senderErr := make(chan string, 1)
	go func() {
		if e := pinThis(sender); e != "" {
			senderErr <- e
			return
		}
		senderErr <- ""
		for cm := range cmds {
			interval := int64(1e9) / int64(cm.rate)
			start := c.now()
			end := start + int64(cm.dur)
			next := start
			var sent int64
			for {
				now := c.now()
				if now >= end {
					break
				}
				if now < next {
					continue
				}
				rawTgkill(pid, tid, unix.SIGURG)
				sent++
				next += interval
				if next < now {
					next = now // no catch-up bursts
				}
			}
			cm.done <- sent
		}
	}()
	if e := <-senderErr; e != "" {
		return nil, fmt.Errorf("sender cpu %d: %s", sender, e)
	}
	defer close(cmds)

	time.Sleep(300 * time.Millisecond) // let every loop settle
	for _, r := range rates {
		for _, l := range loops {
			l.MaxGapAny.Swap(0)
			l.MaxGapWake.Swap(0)
		}
		type snap struct {
			w, e, o, g50 uint64
			ts           thrStat
		}
		take := func() []snap {
			out := make([]snap, len(loops))
			for i, l := range loops {
				out[i] = snap{l.Wakes.Load(), l.Eintr.Load(), l.OtherErr.Load(), l.Gap50.Load(), readThr(int(l.Tid.Load()))}
			}
			return out
		}
		a := take()
		t0 := c.now()
		done := make(chan int64, 1)
		if r > 0 {
			cmds <- cmd{r, dur, done}
		} else {
			go func() { time.Sleep(dur); done <- 0 }()
		}
		rs := make([]int, len(loops))
		ns := 0
		tk := time.NewTicker(dur / 20)
		var sent int64
	wait:
		for {
			select {
			case sent = <-done:
				break wait
			case <-tk.C:
				ns++
				for i, l := range loops {
					if readThr(int(l.Tid.Load())).State == 'R' {
						rs[i]++
					}
				}
			}
		}
		tk.Stop()
		t1 := c.now()
		b := take()
		ph := e3Phase{Rate: r, Sent: sent, Secs: float64(t1-t0) / 1e9}
		ph.SentPerS = float64(sent) / ph.Secs
		for i, l := range loops {
			gAny := max(l.MaxGapAny.Swap(0), t1-l.LastAny.Load())
			gWake := max(l.MaxGapWake.Swap(0), t1-l.LastWake.Load())
			role := "bystander"
			if i == 0 {
				role = "target"
			}
			ph.Loops = append(ph.Loops, e3Loop{
				CPU: l.CPU, Role: role, Wakes: b[i].w - a[i].w, Eintr: b[i].e - a[i].e, OthErr: b[i].o - a[i].o,
				GapAnyNs: gAny, GapWakeNs: gWake, Gap50: b[i].g50 - a[i].g50, RSamples: rs[i], Samples: ns,
				Ut: b[i].ts.Ut - a[i].ts.Ut, St: b[i].ts.St - a[i].ts.St, Vol: b[i].ts.Vol - a[i].ts.Vol, Invol: b[i].ts.Invol - a[i].ts.Invol,
			})
		}
		res.Phases = append(res.Phases, ph)
		time.Sleep(200 * time.Millisecond)
	}
	return res, nil
}

func medianF(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	slices.Sort(s)
	return s[len(s)/2]
}

// e3Plan picks, for each class, the loop CPUs, the signalled target (the first)
// and the sender CPU.
func e3Plan(tp topo, nLoops int) (plans []struct {
	Class       string
	Loops       []int
	Sender      int
	SenderClass string
}) {
	sender := tp.byFastness(tp.Classes[0])[0]
	for _, g := range tp.Classes {
		var cand []int
		for _, c := range sortedCopy(g) {
			if c != sender {
				cand = append(cand, c)
			}
		}
		if len(cand) == 0 {
			continue
		}
		plans = append(plans, struct {
			Class       string
			Loops       []int
			Sender      int
			SenderClass string
		}{classLabel(tp, g[0]), takeN(cand, nLoops), sender, classLabel(tp, sender)})
	}
	return
}

func TestProbeE3SigInject(t *testing.T) {
	tp := loadTopo()
	logf(t, "E3 SIGURG injector, no GC\nhost: %s\ntopology:\n%s", hostFacts(), tp.describe())
	nLoops := max(envInt("CELERIS_PROBE_E3_LOOPS", 4), 2)
	secs := envInt("CELERIS_PROBE_E3_SECS", 3)
	rates := envList("CELERIS_PROBE_E3_RATES", "1000,10000,50000,100000,200000")
	plans := e3Plan(tp, nLoops)
	if len(plans) == 0 {
		t.Skip("no CPU to run a loop on besides the sender")
	}
	var sums []string
	for _, p := range plans {
		budget := time.Duration((len(rates)+1)*(secs+1)+90) * time.Second
		env := []string{
			"GODEBUG=asyncpreemptoff=0",
			"CELERIS_PROBE_ARG_E3_LOOPS=" + fmtCPUs(p.Loops),
			"CELERIS_PROBE_ARG_E3_SENDER=" + strconv.Itoa(p.Sender),
		}
		so, se, err := runChild(childSpec{kind: "e3", env: env, timeout: budget})
		if err != nil {
			t.Fatalf("class %s: the child failed: %v\n%s", p.Class, err, tailLines(se, 20))
		}
		var res e3Result
		if err := decodeChild(so, &res); err != nil {
			t.Fatalf("class %s: %v\n%s", p.Class, err, tailLines(se, 20))
		}
		sums = append(sums, reportE3(t, tp, p.Class, p.SenderClass, res))
	}
	logf(t, "E3 SUMMARY %s: %s", runtime.GOARCH, strings.Join(sums, " || "))
}

func reportE3(t *testing.T, tp topo, class, senderClass string, res e3Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "E3 class %s: loop CPUs %s (signalled: cpu %d; the rest are bystanders); sender cpu %d (%s); child: %s\n", class, fmtCPUs(res.CPUs), res.Target, res.Sender, senderClass, res.Facts)
	fmt.Fprintf(&b, "  rate/s  sent/s  | TARGET ret/s  tmo/s    eintr/s  gap_any_ms gap_tmo_ms R%%   usr%%  sys%%  invol/s | BYSTANDERS(median) ret/s  gap_any_ms gap_tmo_ms R%%   sys%%")
	var stall, coal *e3Phase
	var baseRet float64
	var stallRet, coalRet float64
	for i := range res.Phases {
		ph := &res.Phases[i]
		tg := ph.Loops[0]
		ret := float64(tg.Wakes+tg.Eintr) / ph.Secs
		tmo := float64(tg.Wakes) / ph.Secs
		ein := float64(tg.Eintr) / ph.Secs
		rp := 100 * float64(tg.RSamples) / float64(max(tg.Samples, 1))
		usr := float64(tg.Ut) / 100 / ph.Secs * 100
		sys := float64(tg.St) / 100 / ph.Secs * 100
		var bret, bgapA, bgapW, br, bsys []float64
		for _, l := range ph.Loops[1:] {
			bret = append(bret, float64(l.Wakes+l.Eintr)/ph.Secs)
			bgapA = append(bgapA, float64(l.GapAnyNs)/1e6)
			bgapW = append(bgapW, float64(l.GapWakeNs)/1e6)
			br = append(br, 100*float64(l.RSamples)/float64(max(l.Samples, 1)))
			bsys = append(bsys, float64(l.St)/100/ph.Secs*100)
		}
		fmt.Fprintf(&b, "\n  %-7d %-7.0f | %-17.0f %-8.0f %-8.0f %-10.1f %-10.1f %-4.0f %-5.0f %-5.0f %-8.0f | %-17.0f %-10.1f %-10.1f %-4.0f %.0f",
			ph.Rate, ph.SentPerS, ret, tmo, ein, float64(tg.GapAnyNs)/1e6, float64(tg.GapWakeNs)/1e6, rp, usr, sys, float64(tg.Invol)/ph.Secs,
			medianF(bret), medianF(bgapA), medianF(bgapW), medianF(br), medianF(bsys))
		if ph.Rate == 0 {
			baseRet = ret
			continue
		}
		if stall == nil && (float64(tg.GapAnyNs) >= 100e6 || ret < 0.5*baseRet) {
			stall, stallRet = ph, ret
		}
		if coal == nil && ret < 0.5*ph.SentPerS {
			coal, coalRet = ph, ret
		}
	}
	last := res.Phases[len(res.Phases)-1]
	rstall := fmt.Sprintf("none up to %d/s", last.Rate)
	if stall != nil {
		rstall = fmt.Sprintf("%d/s (target ret/s %.0f vs %.0f at r=0)", stall.Rate, stallRet, baseRet)
	}
	rcoal := fmt.Sprintf("none up to %d/s", last.Rate)
	if coal != nil {
		rcoal = fmt.Sprintf("%d/s (a SLEEPING target absorbs %.0f EINTR cycles/s = %.1f us per cycle of wake, handler, return, re-enter wait; this is not the running-target latency that E0 compares with 5 us)", coal.Rate, coalRet, 1e6/max(coalRet, 1))
	}
	fmt.Fprintf(&b, "\n  r*stall %s; r*coalesce %s", rstall, rcoal)
	logf(t, "%s", b.String())
	return fmt.Sprintf("%s r*stall %s, r*coalesce %s, achieved %.0f/s at the top rate", class, rstall, rcoal, last.SentPerS)
}
