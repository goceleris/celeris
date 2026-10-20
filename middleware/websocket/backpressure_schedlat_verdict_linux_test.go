//go:build linux

package websocket

import (
	"math"
	"runtime/metrics"
	"strings"
	"testing"
	"time"
)

// wsoFixedSched is a probe that reports one scheduler latency, clipped to the
// window as the real probe clips it: the seam of the discount, for a test that
// needs a latency the runtime will not produce on demand.
type wsoFixedSched time.Duration

func (wsoFixedSched) observe(int64) {}
func (p wsoFixedSched) maxIn(fromNs, endNs, windowNs int64) time.Duration {
	return min(time.Duration(p), time.Duration(max(endNs-fromNs, 0)))
}
func (p wsoFixedSched) coveredIn(fromNs, endNs, windowNs int64) time.Duration {
	return p.maxIn(fromNs, endNs, windowNs)
}

// TestWSOStallSchedulerLatencyDiscount drives watchSample on a fake clock
// (celeris#871): a stretch of unread bytes at a socket the engine does not read,
// with scheduler latency recorded inside it, is excused up to
// wsoSchedDiscountCap and no further; latency from outside its window excuses
// nothing. The negative controls (no latency; latency from another window)
// still fail the stretch. Every case ends the stretch with a moved view so it
// is judged on its whole window, except "open" ones, which are judged live.
func TestWSOStallSchedulerLatencyDiscount(t *testing.T) {
	ms := int64(time.Millisecond)
	secs := func(n int64) int64 { return n * 1000 }
	stall := func(raw int64) wsoSrvView {
		return wsoSrvView{found: true, sockOK: true, phase: 1, inq: 1, rawRead: raw}
	}
	type ev struct {
		atMs int64
		lat  time.Duration
	}
	type seg struct {
		fromMs, toMs int64 // samples every wsoWatchEvery from fromMs to toMs
		raw          int64 // the view's bytes read
	}
	cases := []struct {
		name          string
		segs          []seg
		evs           []ev
		endMs         int64 // the sample that moves the view and ends the last stretch; 0: none (left open)
		wantStalls    int
		wantOpen      bool // the failing record was made while the stretch was open (before any move)
		wantDiscount  int  // stretches excused
		preload       bool // every event is in the probe from the start, even one after the judgment
		wantEffective time.Duration
	}{
		// The failing-first case of celeris#871: 6 s of stall with 3 s of
		// scheduler latency in it. The old oracle fails it (5 s limit); net of
		// the latency it is 3 s.
		{name: "6 s stall with 3 s of scheduler latency inside is excused",
			segs: []seg{{0, secs(6), 7}}, evs: []ev{{secs(6), 3 * time.Second}}, endMs: secs(6) + 250,
			wantStalls: 0, wantDiscount: 1},
		{name: "negative control: the same stall with no scheduler latency fails",
			segs: []seg{{0, secs(6), 7}}, endMs: secs(6) + 250,
			wantStalls: 1, wantEffective: 6 * time.Second},
		{name: "latency that leaves 5 s or more still fails",
			segs: []seg{{0, secs(9), 7}}, evs: []ev{{secs(9), 3 * time.Second}}, endMs: secs(9) + 250,
			wantStalls: 1, wantEffective: 6 * time.Second},
		{name: "latency that brings it just under the limit is excused",
			segs: []seg{{0, secs(8), 7}}, evs: []ev{{secs(8), 3500 * time.Millisecond}}, endMs: secs(8) + 250,
			wantStalls: 0, wantDiscount: 1},
		// The cap: a parked reader still fails whatever the scheduler reports.
		{name: "20 s stall with 30 s of reported latency fails: the discount is capped",
			segs: []seg{{0, secs(20), 7}}, evs: []ev{{secs(20), 30 * time.Second}}, endMs: secs(20) + 250,
			wantStalls: 1, wantOpen: true, wantEffective: 10 * time.Second},
		{name: "12 s stall with 30 s of reported latency is excused: effective 2 s",
			segs: []seg{{0, secs(12), 7}}, evs: []ev{{secs(12), 30 * time.Second}}, endMs: secs(12) + 250,
			wantStalls: 0, wantDiscount: 1},
		{name: "14.75 s stall with 30 s of reported latency is excused: effective 4.75 s",
			segs: []seg{{0, 14750, 7}}, evs: []ev{{14750, 30 * time.Second}}, endMs: 15000,
			wantStalls: 0, wantDiscount: 1},
		// The window: latency measured elsewhere does not excuse this stretch.
		{name: "latency from before the stretch's window excuses nothing",
			// view 7 for 0-1.75 s, then view 8 from 2 s for 6 s; the 3 s event is at 1.5 s
			segs: []seg{{0, 1750, 7}, {2000, 8000, 8}}, evs: []ev{{1500, 3 * time.Second}}, endMs: 8250,
			wantStalls: 1, wantEffective: 6 * time.Second},
		{name: "latency recorded after the stretch ended excuses nothing",
			segs: []seg{{0, secs(6), 7}}, evs: []ev{{secs(9), 3 * time.Second}}, endMs: secs(6) + 250, preload: true,
			wantStalls: 1, wantEffective: 6 * time.Second},
		{name: "a long wait is clipped to the part inside the window",
			// the 30 s wait ended 0.5 s into a stretch that began at 2 s: 0.5 s counts
			segs: []seg{{0, 1750, 7}, {2000, 8000, 8}}, evs: []ev{{2500, 30 * time.Second}}, endMs: 8250,
			wantStalls: 1, wantEffective: 5500 * time.Millisecond},
		{name: "a stretch under the limit is not a stall and not excused",
			segs: []seg{{0, secs(4), 7}}, endMs: secs(4) + 250,
			wantStalls: 0},
		// Judged live: a stretch still open fails when even the largest discount would not save it.
		{name: "open stretch past limit+cap fails live, whatever the probe says",
			segs: []seg{{0, secs(15), 7}}, evs: []ev{{secs(15), 60 * time.Second}},
			wantStalls: 1, wantOpen: true, wantEffective: 5 * time.Second},
		{name: "open stretch under limit+cap is not failed live",
			segs:       []seg{{0, 14750, 7}},
			wantStalls: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sched := &wsoSchedLat{}
			g := &wsoRig{t0: time.Now(), sched: sched}
			s := &wsoSrv{g: g, port: 40010}
			w := newWSOWatchState()
			// The probe is fed as the real one is: an event at a pass, when
			// the pass runs, so it is in place for the samples of that pass
			// and after it.
			evs := append([]ev(nil), tc.evs...)
			feed := func(atMs int64) {
				for len(evs) > 0 && evs[0].atMs <= atMs {
					sched.add(evs[0].atMs*ms, evs[0].lat)
					evs = evs[1:]
				}
			}
			if tc.preload {
				for _, e := range evs {
					sched.add(e.atMs*ms, e.lat)
				}
				evs = nil
			}
			var liveStalls int
			for _, sg := range tc.segs {
				for at := sg.fromMs; at <= sg.toMs; at += int64(wsoWatchEvery / time.Millisecond) {
					feed(at)
					g.watchSample(w, s, stall(sg.raw), at*ms)
				}
			}
			liveStalls = len(w.stalls)
			if tc.endMs > 0 {
				feed(tc.endMs)
				// the view moves: the stretch ends and is judged on its window
				g.watchSample(w, s, stall(99), tc.endMs*ms)
			}
			if len(w.stalls) != tc.wantStalls {
				t.Fatalf("stalls = %d, want %d (discounted %d)", len(w.stalls), tc.wantStalls, g.stallDiscounted)
			}
			if tc.wantOpen && liveStalls != 1 {
				t.Fatalf("the stall was not recorded while the stretch was open (live stalls = %d)", liveStalls)
			}
			if tc.wantStalls == 1 {
				st := w.stalls[0]
				if st.effective != tc.wantEffective {
					t.Fatalf("effective stall = %v (stretch %v, sched %v, discount %v), want %v",
						st.effective, time.Duration(st.toNs-st.fromNs), st.sched, st.discount, tc.wantEffective)
				}
			}
			if g.stallDiscounted != tc.wantDiscount {
				t.Fatalf("stallDiscounted = %d, want %d", g.stallDiscounted, tc.wantDiscount)
			}
			if tc.wantDiscount > 0 && g.stallDiscountedMax < wsoRecvStall {
				t.Fatalf("stallDiscountedMax = %v, want at least the limit", g.stallDiscountedMax)
			}
		})
	}

	t.Run("a stretch open when the watch stops is judged then", func(t *testing.T) {
		sched := &wsoSchedLat{}
		g := &wsoRig{t0: time.Now(), sched: sched}
		s := &wsoSrv{g: g, port: 40011}
		w := newWSOWatchState()
		for at := int64(0); at <= 7000; at += 250 {
			g.watchSample(w, s, stall(7), at*ms)
		}
		g.watchSettleAll(w, 7250*ms, true)
		if len(w.stalls) != 1 || !w.stalls[0].open {
			t.Fatalf("stalls = %d, want one, still going when the watch stopped", len(w.stalls))
		}
		// and with 3 s of latency in its window the same stretch is excused
		sched2 := &wsoSchedLat{}
		sched2.add(7000*ms, 3*time.Second)
		g2 := &wsoRig{t0: time.Now(), sched: sched2}
		s2 := &wsoSrv{g: g2, port: 40012}
		w2 := newWSOWatchState()
		for at := int64(0); at <= 7000; at += 250 {
			g2.watchSample(w2, s2, stall(7), at*ms)
		}
		g2.watchSettleAll(w2, 7250*ms, true)
		if len(w2.stalls) != 0 || g2.stallDiscounted != 1 {
			t.Fatalf("stalls = %d discounted = %d, want 0 and 1", len(w2.stalls), g2.stallDiscounted)
		}
	})

	t.Run("a nil probe is the old oracle", func(t *testing.T) {
		g := &wsoRig{t0: time.Now()}
		s := &wsoSrv{g: g, port: 40013}
		w := newWSOWatchState()
		for at := int64(0); at <= 5000; at += 250 {
			g.watchSample(w, s, stall(7), at*ms)
		}
		g.watchSample(w, s, stall(8), 5250*ms)
		if len(w.stalls) != 1 {
			t.Fatalf("stalls = %d, want 1: a stretch of 5 s with no probe fails at the limit", len(w.stalls))
		}
	})
}

// TestWSOStallSchedulerLatencyAfterTheProvenSpan is the celeris#871 review's
// finding: a stretch is excused only by scheduler latency measured inside its
// proven span (first to last sample). Latency recorded by the pass that found
// the stretch over, after its last sample (the watcher's own starved wait), is
// not inside it: the three ways a stretch ends without a read moving the view,
// each with a wait that lies after the span, must still fail it. The control
// of each (the same wait recorded inside the span) is excused.
func TestWSOStallSchedulerLatencyAfterTheProvenSpan(t *testing.T) {
	ms := int64(time.Millisecond)
	stall := func(raw int64) wsoSrvView {
		return wsoSrvView{found: true, sockOK: true, phase: 1, inq: 1, rawRead: raw}
	}
	sample := func(g *wsoRig, w *wsoWatchState, s *wsoSrv, fromMs, toMs, raw int64) {
		for at := fromMs; at <= toMs; at += int64(wsoWatchEvery / time.Millisecond) {
			g.watchSample(w, s, stall(raw), at*ms)
		}
	}
	// the ends: how the stretch is found over, one pass after its last sample
	ends := []struct {
		name   string
		endMs  int64 // the ending pass
		finish func(g *wsoRig, w *wsoWatchState, s *wsoSrv, atMs int64)
	}{
		{"gap restart: the next sample comes after the watcher's starved wait", 7250,
			func(g *wsoRig, w *wsoWatchState, s *wsoSrv, atMs int64) { g.watchSample(w, s, stall(7), atMs*ms) }},
		{"watcher reset: the pass after a starved tick settles every stretch", 7250,
			func(g *wsoRig, w *wsoWatchState, s *wsoSrv, atMs int64) { g.watchSettleAll(w, atMs*ms, false) }},
		{"the watcher starved for 4 s after an 8.75 s stretch", 12750,
			func(g *wsoRig, w *wsoWatchState, s *wsoSrv, atMs int64) { g.watchSettleAll(w, atMs*ms, false) }},
	}
	for _, e := range ends {
		provenEndMs := int64(5750)
		if e.endMs == 12750 {
			provenEndMs = 8750
		}
		wait := time.Duration(e.endMs-provenEndMs) * time.Millisecond // the starved wait, wholly after the span
		t.Run(e.name+": a wait after the last sample does not excuse it", func(t *testing.T) {
			sched := &wsoSchedLat{}
			g := &wsoRig{t0: time.Now(), sched: sched}
			s := &wsoSrv{g: g, port: 40020}
			w := newWSOWatchState()
			sample(g, w, s, 0, provenEndMs, 7)
			sched.add(e.endMs*ms, wait) // recorded by the ending pass: [provenEnd, endMs]
			e.finish(g, w, s, e.endMs)
			if len(w.stalls) != 1 || g.stallDiscounted != 0 {
				t.Fatalf("stalls = %d discounted = %d, want 1 and 0: a %v wait after the span excused it", len(w.stalls), g.stallDiscounted, wait)
			}
			if st := w.stalls[0]; st.sched != 0 || st.covered != 0 || st.effective != time.Duration(provenEndMs)*time.Millisecond {
				t.Fatalf("sched = %v covered = %v effective = %v, want 0, 0 and the whole stretch %v (the record's own figures must not count the wait after the span either)",
					st.sched, st.covered, st.effective, time.Duration(provenEndMs)*time.Millisecond)
			}
		})
		t.Run(e.name+": control, the same wait inside the span excuses it", func(t *testing.T) {
			sched := &wsoSchedLat{}
			g := &wsoRig{t0: time.Now(), sched: sched}
			s := &wsoSrv{g: g, port: 40021}
			w := newWSOWatchState()
			sample(g, w, s, 0, provenEndMs, 7)
			sched.add(provenEndMs*ms, wait) // the same wait, ended at the last sample
			e.finish(g, w, s, e.endMs)
			if len(w.stalls) != 0 || g.stallDiscounted != 1 {
				t.Fatalf("stalls = %d discounted = %d, want 0 and 1", len(w.stalls), g.stallDiscounted)
			}
		})
	}
	t.Run("a wait that straddles the last sample counts up to it", func(t *testing.T) {
		sched := &wsoSchedLat{}
		g := &wsoRig{t0: time.Now(), sched: sched}
		s := &wsoSrv{g: g, port: 40022}
		w := newWSOWatchState()
		sample(g, w, s, 0, 5750, 7)
		sched.add(7250*ms, 2*time.Second) // [5.25, 7.25]: 0.5 s inside the span
		g.watchSample(w, s, stall(7), 7250*ms)
		if len(w.stalls) != 1 || w.stalls[0].sched != 500*time.Millisecond {
			t.Fatalf("stalls = %d, want 1 with sched 500ms (got %v)", len(w.stalls), func() any {
				if len(w.stalls) > 0 {
					return w.stalls[0].sched
				}
				return nil
			}())
		}
	})
	t.Run("an excused stretch is listed", func(t *testing.T) {
		sched := &wsoSchedLat{}
		g := &wsoRig{t0: time.Now(), sched: sched}
		s := &wsoSrv{g: g, port: 40023}
		w := newWSOWatchState()
		sample(g, w, s, 0, 6000, 7)
		sched.add(6000*ms, 3*time.Second)
		g.watchSample(w, s, stall(99), 6250*ms)
		if out := g.longestStretch(); !strings.Contains(out, "excused by scheduler latency: 1 stretch") ||
			!strings.Contains(out, "conn=127.0.0.1:40023 stretch 6.000s") {
			t.Fatalf("an excused stretch is not listed:\n%s", out)
		}
	})
}

// TestWSOJudgeStretch pins the rule's arithmetic.
func TestWSOJudgeStretch(t *testing.T) {
	cases := []struct {
		proven, sched time.Duration
		discount, eff time.Duration
		fails         bool
	}{
		{6 * time.Second, 0, 0, 6 * time.Second, true},
		{6 * time.Second, 3 * time.Second, 3 * time.Second, 3 * time.Second, false},
		{4999 * time.Millisecond, 0, 0, 4999 * time.Millisecond, false},
		{5 * time.Second, 0, 0, 5 * time.Second, true}, // the limit itself fails: >=
		{8 * time.Second, 3 * time.Second, 3 * time.Second, 5 * time.Second, true},
		{15 * time.Second, time.Hour, wsoSchedDiscountCap, 5 * time.Second, true}, // the cap
		{15*time.Second - 1, time.Hour, wsoSchedDiscountCap, 5*time.Second - 1, false},
		{6 * time.Second, -time.Second, 0, 6 * time.Second, true}, // a negative latency is nothing
	}
	for _, tc := range cases {
		d, e, f := wsoJudgeStretch(tc.proven, tc.sched)
		if d != tc.discount || e != tc.eff || f != tc.fails {
			t.Errorf("wsoJudgeStretch(%v, %v) = %v, %v, %v; want %v, %v, %v", tc.proven, tc.sched, d, e, f, tc.discount, tc.eff, tc.fails)
		}
	}
	if wsoStretchFailsLive(15*time.Second-1) || !wsoStretchFailsLive(15*time.Second) {
		t.Errorf("wsoStretchFailsLive must switch at wsoRecvStall + wsoSchedDiscountCap = %v", wsoRecvStall+wsoSchedDiscountCap)
	}
}

// TestWSOSchedLatMaxIn pins the window of the real probe's events.
func TestWSOSchedLatMaxIn(t *testing.T) {
	ms := int64(time.Millisecond)
	p := &wsoSchedLat{}
	p.add(1000*ms, 800*time.Millisecond)
	p.add(3000*ms, 2*time.Second)
	p.add(6000*ms, 5*time.Second)
	cases := []struct {
		name     string
		from, to int64 // ms
		want     time.Duration
	}{
		{"all three, the last clipped to its window (5 s event, 3 s inside)", 3000, 6000, 3 * time.Second},
		{"an event at the window's start is outside it", 1000, 2999, 0},
		{"an event at the window's end is inside it", 0, 1000, time.Second - 200*time.Millisecond},
		{"an event after the window is outside it", 0, 999, 0},
		{"two inside, the longer clipped", 2000, 3000, time.Second},
		{"the whole run", 0, 10000, 5 * time.Second},
	}
	for _, tc := range cases {
		if got := p.maxIn(tc.from*ms, tc.to*ms, tc.to*ms); got != tc.want {
			t.Errorf("%s: maxIn(%d, %d) = %v, want %v", tc.name, tc.from, tc.to, got, tc.want)
		}
	}
	var nilp *wsoSchedLat
	nilp.observe(0) // a typed-nil probe in the interface must not panic
	if nilp.maxIn(0, 1, 1) != 0 || nilp.longest() != 0 {
		t.Error("a nil probe must report nothing")
	}
	if p.longest() != 5*time.Second {
		t.Errorf("longest = %v, want 5s", p.longest())
	}
}

// TestWSOSchedLatMaxInProvenEnd pins the clip against the end of the span being
// judged (celeris#871 review): events are read up to the window, the pass that
// found the stretch over, but each wait counts only for the part inside
// (from, end].
func TestWSOSchedLatMaxInProvenEnd(t *testing.T) {
	ms := int64(time.Millisecond)
	cases := []struct {
		name              string
		at                int64 // ms; the event's pass
		lat               time.Duration
		from, end, window int64 // ms
		want              time.Duration
	}{
		{"a wait recorded after the proven end, wholly after it, counts for nothing", 12750, 4 * time.Second, 0, 8750, 12750, 0},
		{"a wait that straddles the proven end counts up to it", 9750, 2 * time.Second, 0, 8750, 9750, time.Second},
		{"a wait wholly inside the span counts in full", 6000, 3 * time.Second, 0, 8750, 9000, 3 * time.Second},
		{"the window is the pass that found it: an event after it is not looked at", 9750, 2 * time.Second, 0, 8750, 9000, 0},
		{"a long wait is clipped at both ends", 12000, 30 * time.Second, 2000, 8000, 12000, 6 * time.Second},
		{"an event that ended exactly at the end counts in full", 8000, 3 * time.Second, 0, 8000, 8250, 3 * time.Second},
	}
	for _, tc := range cases {
		p := &wsoSchedLat{}
		p.add(tc.at*ms, tc.lat)
		if got := p.maxIn(tc.from*ms, tc.end*ms, tc.window*ms); got != tc.want {
			t.Errorf("%s: maxIn(%d, %d, %d) = %v, want %v", tc.name, tc.from, tc.end, tc.window, got, tc.want)
		}
		if got := p.coveredIn(tc.from*ms, tc.end*ms, tc.window*ms); got != tc.want {
			t.Errorf("%s: coveredIn(%d, %d, %d) = %v, want %v", tc.name, tc.from, tc.end, tc.window, got, tc.want)
		}
	}
}

// TestWSOSchedLatCoveredIn pins the informational figure: the union of the
// waits' intervals inside the window, so overlapping waits are counted once and
// waits one after another add up.
func TestWSOSchedLatCoveredIn(t *testing.T) {
	ms := int64(time.Millisecond)
	p := &wsoSchedLat{}
	p.add(2000*ms, 1500*time.Millisecond) // [0.5, 2.0]
	p.add(2250*ms, 1500*time.Millisecond) // [0.75, 2.25]: overlaps the first
	p.add(5000*ms, 1000*time.Millisecond) // [4.0, 5.0]: a separate wait
	cases := []struct {
		from, to int64
		want     time.Duration
	}{
		{0, 10000, 1750*time.Millisecond + time.Second}, // [0.5,2.25] + [4,5]
		{1000, 10000, 1250*time.Millisecond + time.Second},
		{0, 2000, 1500 * time.Millisecond},
		{2500, 10000, time.Second},
		{0, 100, 0},
	}
	// a wait that contains an earlier, shorter one: the union is the longer
	q := &wsoSchedLat{}
	q.add(5000*ms, time.Second)   // [4, 5]
	q.add(6000*ms, 6*time.Second) // [0, 6]
	if got := q.coveredIn(0, 10000*ms, 10000*ms); got != 6*time.Second {
		t.Errorf("nested waits: coveredIn = %v, want 6s", got)
	}
	for _, tc := range cases {
		if got := p.coveredIn(tc.from*ms, tc.to*ms, tc.to*ms); got != tc.want {
			t.Errorf("coveredIn(%d, %d) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// TestWSOHistMaxDelta pins how a histogram's growth becomes a latency: the
// lower bound of the highest bucket whose count grew.
func TestWSOHistMaxDelta(t *testing.T) {
	inf := math.Inf(1)
	buckets := []float64{math.Inf(-1), 0, 0.001, 0.5, 2.5, 3.1, inf} // 6 buckets: counts[i] covers [buckets[i], buckets[i+1])
	cases := []struct {
		name      string
		prev, cur []uint64
		want      time.Duration
	}{
		{"no growth", []uint64{1, 1, 1, 1, 1, 1}, []uint64{1, 1, 1, 1, 1, 1}, 0},
		{"growth in a middle bucket", []uint64{0, 0, 0, 0, 0, 0}, []uint64{0, 4, 9, 0, 0, 0}, time.Millisecond},
		{"the highest grown bucket wins", []uint64{0, 0, 0, 0, 0, 0}, []uint64{0, 4, 9, 1, 1, 0}, 2500 * time.Millisecond},
		{"the top bucket's lower bound, never its +Inf upper", []uint64{0, 0, 0, 0, 0, 0}, []uint64{0, 0, 0, 0, 0, 1}, 3100 * time.Millisecond},
		{"a bucket that did not grow is not counted", []uint64{0, 0, 0, 7, 7, 7}, []uint64{0, 1, 0, 7, 7, 7}, 0},
		{"growth in the -Inf bucket has no finite lower bound", []uint64{0, 0, 0, 0, 0, 0}, []uint64{1, 0, 0, 0, 0, 0}, 0},
		{"growth in the 0 bucket is zero", []uint64{0, 0, 0, 0, 0, 0}, []uint64{0, 1, 0, 0, 0, 0}, 0},
		{"no previous snapshot: any count is growth", nil, []uint64{0, 0, 0, 2, 0, 0}, 500 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := wsoHistMaxDelta(tc.prev, tc.cur, buckets); got != tc.want {
			t.Errorf("%s: wsoHistMaxDelta = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestWSOSchedLatReadsTheRuntime checks that the real probe's metric exists in
// this runtime and that a read is a histogram, so a runtime that renamed it
// fails here instead of silently discounting nothing.
func TestWSOSchedLatReadsTheRuntime(t *testing.T) {
	found := false
	for _, d := range metrics.All() {
		if d.Name == wsoSchedMetric {
			found = true
			if d.Kind != metrics.KindFloat64Histogram {
				t.Fatalf("%s is %v, want a Float64Histogram", d.Name, d.Kind)
			}
		}
	}
	if !found {
		t.Fatalf("runtime/metrics has no %s: the scheduler-latency discount would never apply", wsoSchedMetric)
	}
	p := newWSOSchedLat()
	if p.prev == nil {
		t.Fatal("the baseline snapshot of the histogram was not taken")
	}
	p.observe(1)
	if p.longest() < 0 {
		t.Fatal("negative latency")
	}
}
