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

// The latency of the std and HTTP/2 fallback (a stream no engine stamped:
// handler.go stamps it with mono.FromTime of the time.Now() it takes) is the same.
func TestFallbackLatencyIgnoresAStepOfTheWallClock1178(t *testing.T) {
	var st mono.Stepper
	defer mono.SetStepper(&st)()

	srv := New(Config{})
	got := make(chan time.Duration, 1)
	srv.GET("/d", func(c *Context) error {
		c.OnRelease(func() {
			if snap := c.detachSnap; snap != nil {
				got <- snap.elapsed
			} else {
				got <- -1
			}
		})
		done := c.Detach()
		go func() {
			time.Sleep(20 * time.Millisecond)
			st.Add(75 * time.Second)
			done()
		}()
		return nil
	})
	adapter := &routerAdapter{server: srv}
	s, _ := newTestStream("GET", "/d") // StartTimeNs 0: the fallback
	defer s.Release()
	if err := adapter.HandleStream(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	select {
	case el := <-got:
		if el < 20*time.Millisecond || el > 5*time.Second {
			t.Errorf("the fallback's elapsed = %v, want about 20 ms", el)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the Context was not released")
	}
}
