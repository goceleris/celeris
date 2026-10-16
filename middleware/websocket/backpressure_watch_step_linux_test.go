//go:build linux

package websocket

import (
	"testing"
	"time"
)

// TestWSOWatchStepFakeViews drives wsoWatchStep with fake views, so the episode
// rule of watch is checked without an engine. Each step is a sample of the view
// v taken at the time at (ms). The expectations are the episode left open after
// the last step (wantFrom, -1 when none), how many steps opened an episode, and
// the samples that episode holds, and how many steps restarted a stretch for the
// gap alone (the watch's watchGapRestarts).
func TestWSOWatchStepFakeViews(t *testing.T) {
	ms := int64(time.Millisecond)
	stall := func(raw int64) wsoSrvView {
		return wsoSrvView{found: true, sockOK: true, phase: 1, inq: 1, rawRead: raw}
	}
	notStallable := func(raw int64) wsoSrvView {
		v := stall(raw)
		v.inq = 0 // nothing unread on the socket: not a stretch
		return v
	}
	type step struct {
		at int64 // ms
		v  wsoSrvView
	}
	run := func(steps []step) (*wsoEpisode, int, int) {
		var ep *wsoEpisode
		opened, gapRestarts := 0, 0
		for _, s := range steps {
			var isNew, gap bool
			ep, isNew, gap = wsoWatchStep(ep, s.v, s.at*ms)
			if isNew {
				opened++
			}
			if gap {
				gapRestarts++
			}
		}
		return ep, opened, gapRestarts
	}
	// steady returns one sample every wsoWatchEvery from 0 to upto ms, with the
	// same view throughout.
	steady := func(upto int64, raw int64) []step {
		var out []step
		for at := int64(0); at <= upto; at += int64(wsoWatchEvery / time.Millisecond) {
			out = append(out, step{at, stall(raw)})
		}
		return out
	}
	evg := int64(wsoWatchEvery+wsoWatchGap) / ms // the allowance between two samples, ms
	cases := []struct {
		name       string
		steps      []step
		wantFrom   int64 // ms, first sample of the open episode; -1 when none
		wantOpened int
		wantFrames int // samples in the open episode
		wantGaps   int // steps that restarted a stretch for the gap alone
	}{
		{"steady stretch keeps its first sample", steady(5000, 7), 0, 1, 21, 0},
		// The watcher was starved for about 5.75 s between two samples: the
		// samples it missed cannot show that the engine read nothing, so the
		// stretch restarts at the sample after the gap.
		{"starved gap restarts the episode", append(steady(250, 7), step{6000, stall(7)}), 6000, 2, 1, 1},
		// A gap just inside the allowance keeps the episode.
		{"gap inside the allowance keeps it", []step{{0, stall(7)}, {evg, stall(7)}}, 0, 1, 2, 0},
		// A gap just past the allowance restarts it.
		{"gap past the allowance restarts it", []step{{0, stall(7)}, {evg + 1, stall(7)}}, evg + 1, 2, 1, 1},
		{"a moved view restarts", []step{{0, stall(7)}, {250, stall(8)}}, 250, 2, 1, 0},
		// A restart the view would have caused anyway is not the gap's.
		{"a moved view after a gap is not a gap restart", []step{{0, stall(7)}, {6000, stall(8)}}, 6000, 2, 1, 0},
		{"a view that is not stallable ends the episode", []step{{0, stall(7)}, {250, notStallable(7)}}, -1, 1, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep, opened, gaps := run(tc.steps)
			if opened != tc.wantOpened {
				t.Fatalf("episodes opened = %d, want %d", opened, tc.wantOpened)
			}
			if gaps != tc.wantGaps {
				t.Fatalf("gap restarts = %d, want %d", gaps, tc.wantGaps)
			}
			if tc.wantFrom < 0 {
				if ep != nil {
					t.Fatalf("an episode is open after a non-stallable view, from %d ms", ep.fromNs/ms)
				}
				return
			}
			if ep == nil {
				t.Fatalf("no episode open, want one from %d ms", tc.wantFrom)
			}
			if ep.fromNs != tc.wantFrom*ms || ep.samples != tc.wantFrames {
				t.Fatalf("open episode from %d ms with %d samples, want from %d ms with %d",
					ep.fromNs/ms, ep.samples, tc.wantFrom, tc.wantFrames)
			}
		})
	}
}
