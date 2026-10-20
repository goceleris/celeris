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
// (runtime/metrics /sched/latencies:seconds, a histogram of every goroutine
// wake-up since the process started). The rig samples it once per watcher
// pass and keeps, for each pass, the longest runnable wait that was recorded
// since the pass before (wsoSchedLat.observe: the lower bound of the highest
// bucket whose count grew, so the figure never overstates a wait).
//
// A stretch is judged over its own window, from its first sample to the pass
// that ended it (the read that moved the view, the connection's end, a
// starved watcher, the watch's stop):
//
//	sched     = the longest runnable wait recorded in that window, each wait
//	            clipped to the part of it that lies inside the window
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
// A stretch is judged when it ends, not while it grows, because the wait of
// the goroutine that was not scheduled is recorded when it finally runs: at
// the moment a stretch first reaches wsoRecvStall the very wait that explains
// it is not in the histogram yet. A stretch that is still open fails at once
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
// scheduler. observe is called once per watcher pass, at rig time nowNs;
// maxIn returns the longest runnable wait recorded in the window
// (fromNs, toNs], each wait clipped to the part inside it. A nil probe
// reports nothing.
type wsoSchedProbe interface {
	observe(nowNs int64)
	maxIn(fromNs, toNs int64) time.Duration
	// coveredIn is informational, never judged: the time inside the window
	// during which some recorded wait was in progress (the union of the waits'
	// intervals). The rule uses the longest single wait; a chain of waits one
	// after another (a lock handed from one runnable goroutine to the next) is
	// longer than any of them, and this is what it adds up to.
	coveredIn(fromNs, toNs int64) time.Duration
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

// maxIn: an event at atNs says a wait of lat ended in the pass ending at atNs,
// so it lay in [atNs-lat, atNs]; the part inside (fromNs, toNs] is at most
// atNs-fromNs. An event after toNs, or at or before fromNs, is not in the
// window.
func (p *wsoSchedLat) maxIn(fromNs, toNs int64) time.Duration {
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
		if e.atNs > toNs {
			continue
		}
		m = max(m, min(e.lat, time.Duration(e.atNs-fromNs)))
	}
	return m
}

func (p *wsoSchedLat) coveredIn(fromNs, toNs int64) time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var covered time.Duration
	end := fromNs // the covered time so far reaches here, scanning the events in time order
	for _, e := range p.evs {
		if e.atNs <= fromNs || e.atNs > toNs {
			continue
		}
		lo := max(e.atNs-int64(e.lat), fromNs, end)
		if e.atNs > lo {
			covered += time.Duration(e.atNs - lo)
		}
		end = max(end, e.atNs)
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
