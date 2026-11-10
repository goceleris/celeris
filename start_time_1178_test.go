package celeris

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/mono"
)

// celeris#1178. A request's start is a stamp of the engines' clock, internal/mono's.
// StartTime() is that instant as a time.Time: the TRUE wall time (the logger prints
// it as the request's timestamp) with a monotonic reading (the logger, metrics and
// otel take time.Since of it as the latency). A conversion that took the stamp for
// a Unix time (time.Unix(0, ns)) is wrong by the distance between the two clocks,
// which the skew canary makes an hour.
func TestStartTimeIsTheTrueWallTimeOfTheRequestsStamp1178(t *testing.T) {
	srv := New(Config{})
	got := make(chan time.Time, 1)
	srv.GET("/s", func(c *Context) error {
		got <- c.StartTime()
		return nil
	})
	adapter := &routerAdapter{server: srv}
	s, _ := newTestStream("GET", "/s")
	defer s.Release()
	s.StartTimeNs = mono.Now() // what an engine does
	before := time.Now()
	if err := adapter.HandleStream(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	select {
	case st := <-got:
		if d := st.Sub(before); d < -time.Second || d > time.Second {
			t.Errorf("StartTime() is %v from the real time of the request", d)
		}
		if age := time.Since(st); age < 0 || age > 5*time.Second {
			t.Errorf("time.Since(StartTime()) = %v, want a few ms", age)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not run")
	}
}

// BenchmarkStartTime1178 is StartTime() of a request an engine stamped: the lazy
// conversion the logger, metrics and otel pay once per request.
func BenchmarkStartTime1178(b *testing.B) {
	b.ReportAllocs()
	c := &Context{}
	ns := mono.Now()
	for range b.N {
		c.startTime = time.Time{}
		c.startTimeNs = ns
		_ = c.StartTime()
	}
}
