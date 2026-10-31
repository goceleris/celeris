package celeris

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/mono"
)

// celeris#1178. A request's start is a stamp of the engines' clock (the native
// engines set Stream.StartTimeNs from it). The latency handed to the metrics
// collector, the elapsed time of the detach snapshot and Context.StartTime()
// (which the logger, metrics and otel take their latency and timestamp from) must
// not count a step of the wall clock during the request. The clock here is the
// seam's: its wall reading steps by 75 s, its monotonic reading runs.

// A step of the wall clock between the request's start and the end of its
// detached handler is not part of the elapsed time of the detach snapshot, and
// StartTime() is the request's true start: within a second of the real wall
// time, and time.Since of it is the request's real age.
func TestRequestLatencyIgnoresAStepOfTheWallClock1178(t *testing.T) {
	var st mono.Stepper
	defer mono.SetStepper(&st)()

	srv := New(Config{})
	type seen struct {
		elapsed time.Duration
		age     time.Duration
		start   time.Time
	}
	got := make(chan seen, 1)
	srv.GET("/d", func(c *Context) error {
		c.OnRelease(func() {
			s := seen{elapsed: -1}
			if snap := c.detachSnap; snap != nil {
				s.elapsed = snap.elapsed
			}
			s.start = c.StartTime()
			s.age = time.Since(s.start)
			got <- s
		})
		done := c.Detach()
		go func() {
			time.Sleep(20 * time.Millisecond)
			st.Add(75 * time.Second) // the step, while the request is in flight
			done()
		}()
		return nil
	})
	adapter := &routerAdapter{server: srv}

	s, _ := newTestStream("GET", "/d")
	defer s.Release()
	s.StartTimeNs = mono.Now()
	started := time.Now()
	if err := adapter.HandleStream(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if r.elapsed < 20*time.Millisecond || r.elapsed > 5*time.Second {
			t.Errorf("the detach snapshot's elapsed = %v, want about 20 ms (a step of 75 s is not request time)", r.elapsed)
		}
		if r.age < 0 || r.age > 5*time.Second {
			t.Errorf("time.Since(StartTime()) = %v, want a few ms (the request's real age)", r.age)
		}
		if d := r.start.Sub(started); d < -time.Second || d > time.Second {
			t.Errorf("StartTime() is %v from the real start of the request", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the Context was not released")
	}
}

// The latency the collector records (handler.go: mono.Now() - c.startTimeNs) is the
// request's, whichever way the request was stamped: by an engine (Stream.StartTimeNs)
// or by the fallback every HTTP/2 and std-engine request takes (mono.FromTime of a
// time.Now() the handler takes). A step of the wall clock while the handler runs is
// not part of it. The detach snapshot above is the detached path; this is the plain
// one. (The fallback's c.startTime is a real time.Now(), so the context's own elapsed
// time (time.Since) never counted a step; the collector's duration is what the flip
// changes, and it is red against the wall clock.)
func TestCollectorLatencyIgnoresAStepOfTheWallClock1178(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stamped bool
	}{{"stamped-by-an-engine", true}, {"fallback", false}} {
		t.Run(tc.name, func(t *testing.T) {
			var st mono.Stepper
			defer mono.SetStepper(&st)()

			srv := New(Config{})
			srv.GET("/p", func(c *Context) error {
				time.Sleep(20 * time.Millisecond)
				st.Add(75 * time.Second) // the step, while the handler runs
				return nil
			})
			adapter := &routerAdapter{server: srv}
			s, _ := newTestStream("GET", "/p")
			defer s.Release()
			if tc.stamped {
				s.StartTimeNs = mono.Now()
			}
			before := srv.Collector().Snapshot()
			if err := adapter.HandleStream(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			snap := srv.Collector().Snapshot()
			if snap.RequestsTotal != before.RequestsTotal+1 {
				t.Fatalf("the collector recorded %d requests, want 1", snap.RequestsTotal-before.RequestsTotal)
			}
			// The request took about 20 ms: its bucket's upper bound is well under a
			// second. 75 s lands in the last bucket (bound 5 s, the overflow).
			for i, n := range snap.LatencyBuckets {
				if n <= before.LatencyBuckets[i] {
					continue
				}
				if i >= len(snap.BucketBounds) || snap.BucketBounds[i] >= 1 {
					t.Errorf("the request was recorded in latency bucket %d (bounds %v): a step of 75 s is not request time", i, snap.BucketBounds)
				}
			}
		})
	}
}
