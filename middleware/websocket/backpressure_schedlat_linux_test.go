//go:build linux

package websocket

// The scheduler-latency discount of the WSO-STALL verdict (celeris#871).
//
// # What the discount is for
//
// The watch (backpressure_oracle_linux_test.go) fails a connection whose
// server socket held unread bytes for wsoRecvStall while nothing asked the
// engine to stop reading. It reads that from the kernel: bytes received minus
// bytes unread did not move. It cannot tell an engine that is not reading
// from an engine that is not RUNNING. At GOMAXPROCS=1 with the default size
// (96 connections, race on) the epoll loop sat in drainRead for 5.6 to 10.7 s
// per stretch in 8 of 8 processes of pristine main: the loop thread was off
// the CPU (0.9% of the wall clock in those calls), the goroutines it waits on
// (the conns' detachMu holders) were runnable and not running, and none was
// parked. That is a loop-service latency of an oversubscribed process, not a
// recv the engine passed over (celeris#607), and wsoWatchGap does not see it:
// the watcher itself is scheduled on time while the loop is not.
//
// # The rule
//
// The Go scheduler reports how long goroutines waited runnable
// (runtime/metrics /sched/latencies:seconds, a histogram since the process
// started). The runtime SAMPLES it: it times the runnable wait of a goroutine
// on one of every gTrackingPeriod (8) of its transitions out of running, not
// on every wake-up (runtime/runtime2.go gTrackingPeriod, proc.go trackingSeq;
// a native count of 1000 long waits added about 1 in 8 histogram entries). So the histogram is a
// lower bound: a long wait of the loop that nobody timed is not in it, and
// "recorded waits cover" below is a sample-based lower bound as well. That
// errs toward fewer excuses, never more. The rig samples it once per watcher
// pass and keeps, for each pass, the longest runnable wait that was recorded
// since the pass before (wsoSchedLat.observe: the lower bound of the highest
// bucket whose count grew, so the figure never overstates a wait).
//
// A stretch is judged over its own proven span, from its first sample to its
// last: the span in which the view provably did not move. The pass that ended
// it (the read that moved the view, the connection's end, a starved watcher, a
// gap restart, the watch's stop) comes later, and the waits it records are
// read up to that pass, because the wait that explains the stretch is only
// recorded when the goroutine runs again; but each wait is credited only for
// the part of it that lies inside the proven span, so the watcher's own
// starvation after the last sample (which that pass records too) excuses
// nothing:
//
//	sched     = the longest runnable wait recorded up to the ending pass, each
//	            wait clipped to the part of it inside (first sample, last sample]
//	discount  = min(sched, wsoSchedDiscountCap)
//	effective = stretch - discount
//	the stretch fails when effective >= wsoRecvStall
//
// So a stretch is excused only by scheduler latency measured inside it, never
// by latency from before it (a different window) and never by more than
// wsoSchedDiscountCap: whatever the scheduler reports, a stretch of
// wsoRecvStall + wsoSchedDiscountCap (15 s) or longer fails. That is the hard
// upper bound, and it is what keeps a reader that is truly parked failing.
// With a probe that reports no latency the rule is the old one: the stretch
// fails at wsoRecvStall.
//
// A stretch is judged when it ends, not while it grows, because a wait is
// recorded only when the goroutine finally runs (and then only if that
// transition was one of the sampled 1 in 8): at the moment a stretch first
// reaches wsoRecvStall the very wait that explains it is not in the histogram
// yet. A stretch that is still open fails at once
// only when even the largest discount would not save it (stretch >=
// wsoRecvStall + wsoSchedDiscountCap), so a run that ends in a timeout still
// prints the record of a truly stuck connection. A stretch that is open when
// the watch stops is judged then.
//
// # What it does not hide
//
// The discount cannot tell which goroutine waited: the histogram has the
// longest wait of any goroutine of the process. A loop that was blocked for
// another reason while some unrelated goroutine waited for the CPU gets the
// same discount; the cap bounds that, and every discounted stretch is
// counted (stallDiscounted) and its longest raw length kept
// (stallDiscountedMax) on the waits line, so a run that was excused is
// visible as one. celeris#607 (a recv chained behind a send) blocks the
// connection while the loop and every goroutine are runnable at once, so it
// records no scheduler latency of its own.

import (
	"math"
	"runtime/metrics"
	"sort"
	"sync"
	"time"
)

// wsoSchedDiscountCap is the most scheduler latency that excuses a stretch.
// Whatever the probe reports, a stretch of wsoRecvStall + wsoSchedDiscountCap
// fails. The longest stretch measured with the default size at GOMAXPROCS=1
// was 10.7 s (celeris#871), so a stretch of that size needs 5.7 s of
// scheduler latency of its own to be excused; one that sat there with no
// latency at all is not.
const wsoSchedDiscountCap = 10 * time.Second

// wsoSchedMetric is the runtime/metrics name of the scheduler-latency
// histogram.
const wsoSchedMetric = "/sched/latencies:seconds"

// wsoSchedProbe is the seam of the discount: what the watcher knows of the Go
// scheduler. observe is called once per watcher pass, at rig time nowNs.
// maxIn returns the longest runnable wait among the events recorded in the
// passes of (fromNs, windowNs], each wait clipped to the part of it inside
// (fromNs, endNs]: endNs is the end of the span being judged (its last
// sample), windowNs the pass that found it ended, so endNs <= windowNs. A
// wait recorded after endNs and starting before it counts only up to endNs;
// one that lay wholly after endNs counts for nothing. A nil probe reports
// nothing.
type wsoSchedProbe interface {
	observe(nowNs int64)
	maxIn(fromNs, endNs, windowNs int64) time.Duration
	// coveredIn is informational, never judged: the time inside
	// (fromNs, endNs] during which some recorded wait was in progress (the
	// union of the waits' intervals; windowNs as in maxIn). The rule uses the
	// longest single wait; a chain of waits one after another (a lock handed
	// from one runnable goroutine to the next) is longer than any of them, and
	// this is what it adds up to. It is a lower bound: see the sampling note
	// at the top of this file.
	coveredIn(fromNs, endNs, windowNs int64) time.Duration
}

// wsoSchedEv is the longest runnable wait recorded in the pass ending at atNs.
type wsoSchedEv struct {
	atNs int64
	lat  time.Duration
}

// wsoSchedLat is the real probe: it reads /sched/latencies:seconds and keeps
// one event per pass that saw a wait. add feeds it events directly, for tests
// that run on a fake clock.
type wsoSchedLat struct {
	mu     sync.Mutex
	sample []metrics.Sample
	prev   []uint64 // bucket counts at the last observe; nil before the first
	evs    []wsoSchedEv
	max    time.Duration // the longest wait of any event
}

// newWSOSchedLat returns a probe whose baseline is now: waits recorded
// before it (the rest of the process's life) are not the rig's.
func newWSOSchedLat() *wsoSchedLat {
	p := &wsoSchedLat{sample: []metrics.Sample{{Name: wsoSchedMetric}}}
	p.observe(0)
	return p
}

// wsoHistMaxDelta returns the lower bound of the highest bucket whose count
// grew from prev to cur (0 if none did, or if that bucket has no finite lower
// bound). Lower, not upper: a wait is at least that long, so the figure never
// excuses more than was observed, and the top bucket's upper bound is +Inf.
// prev may be nil (every count then counts as grown only when above 0).
func wsoHistMaxDelta(prev, cur []uint64, buckets []float64) time.Duration {
	for i := len(cur) - 1; i >= 0; i-- {
		var was uint64
		if i < len(prev) {
			was = prev[i]
		}
		if cur[i] <= was {
			continue
		}
		if i >= len(buckets) {
			return 0
		}
		lo := buckets[i]
		if math.IsInf(lo, 0) || math.IsNaN(lo) || lo <= 0 {
			return 0
		}
		return time.Duration(lo * float64(time.Second))
	}
	return 0
}

func (p *wsoSchedLat) observe(nowNs int64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	metrics.Read(p.sample)
	if p.sample[0].Value.Kind() != metrics.KindFloat64Histogram {
		return // the runtime does not have the metric: no discount
	}
	h := p.sample[0].Value.Float64Histogram()
	// The runtime owns h.Counts and may overwrite them on the next Read.
	cur := append([]uint64(nil), h.Counts...)
	if p.prev != nil {
		if lat := wsoHistMaxDelta(p.prev, cur, h.Buckets); lat > 0 {
			p.addLocked(nowNs, lat)
		}
	}
	p.prev = cur
}

// add records that the longest runnable wait seen in the pass ending at atNs
// was lat.
func (p *wsoSchedLat) add(atNs int64, lat time.Duration) {
	p.mu.Lock()
	p.addLocked(atNs, lat)
	p.mu.Unlock()
}

func (p *wsoSchedLat) addLocked(atNs int64, lat time.Duration) {
	p.evs = append(p.evs, wsoSchedEv{atNs, lat})
	p.max = max(p.max, lat)
}

// wsoCredit is what an event (a wait of lat that ended in the pass ending at
// atNs, so it lay in [atNs-lat, atNs]) adds inside (fromNs, endNs]: the part of
// that interval inside it, 0 if none.
func wsoCredit(atNs int64, lat time.Duration, fromNs, endNs int64) time.Duration {
	return time.Duration(max(0, min(atNs, endNs)-max(fromNs, atNs-int64(lat))))
}

// maxIn: see wsoSchedProbe. An event at or before fromNs, or after windowNs,
// is not looked at; the others are credited with wsoCredit.
func (p *wsoSchedLat) maxIn(fromNs, endNs, windowNs int64) time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var m time.Duration
	for i := len(p.evs) - 1; i >= 0; i-- {
		e := p.evs[i]
		if e.atNs <= fromNs {
			break // events are in time order: the rest are older
		}
		if e.atNs > windowNs {
			continue
		}
		m = max(m, wsoCredit(e.atNs, e.lat, fromNs, endNs))
	}
	return m
}

func (p *wsoSchedLat) coveredIn(fromNs, endNs, windowNs int64) time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	type iv struct{ lo, hi int64 }
	var ivs []iv
	for _, e := range p.evs {
		if e.atNs <= fromNs || e.atNs > windowNs {
			continue
		}
		lo, hi := max(e.atNs-int64(e.lat), fromNs), min(e.atNs, endNs)
		if hi > lo {
			ivs = append(ivs, iv{lo, hi})
		}
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].lo < ivs[j].lo })
	var covered time.Duration
	end := fromNs // the union so far reaches here
	for _, v := range ivs {
		lo := max(v.lo, end)
		if v.hi > lo {
			covered += time.Duration(v.hi - lo)
		}
		end = max(end, v.hi)
	}
	return covered
}

// longest is the longest runnable wait of the whole run.
func (p *wsoSchedLat) longest() time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max
}

// wsoJudgeStretch applies the rule: a stretch of proven length, with sched of
// scheduler latency inside its window, fails when its length less the
// discount (sched, at most wsoSchedDiscountCap) is still wsoRecvStall or more.
func wsoJudgeStretch(proven, sched time.Duration) (discount, effective time.Duration, fails bool) {
	discount = min(max(sched, 0), wsoSchedDiscountCap)
	effective = proven - discount
	return discount, effective, effective >= wsoRecvStall
}

// wsoStretchFailsLive reports whether a stretch that is still open already
// fails: it would even with the largest discount.
func wsoStretchFailsLive(proven time.Duration) bool {
	_, _, fails := wsoJudgeStretch(proven, wsoSchedDiscountCap)
	return fails
}
