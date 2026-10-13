package celeris

import (
	"context"
	"testing"
	"time"
)

// TestDetachSnapshotElapsedFromStartTimeNs889 pins the elapsed time the done
// func of Context.Detach records for the metrics collector. The native engines
// set Stream.StartTimeNs and HandleStream then leaves Context.startTime the
// zero Time (it is materialized lazily by StartTime), so an elapsed computed
// with time.Since(c.startTime) was the maximum Duration, about 292 years.
func TestDetachSnapshotElapsedFromStartTimeNs889(t *testing.T) {
	srv := New(Config{})
	got := make(chan time.Duration, 1)
	srv.GET("/d", func(c *Context) error {
		// OnRelease runs in the release goroutine, after done() closed the
		// channel and before the fields are cleared, so the snapshot is read
		// in the order the metrics goroutine reads it.
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
			done()
		}()
		return nil
	})
	adapter := &routerAdapter{server: srv}

	s, _ := newTestStream("GET", "/d")
	defer s.Release()
	s.StartTimeNs = time.Now().UnixNano()
	if err := adapter.HandleStream(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	select {
	case el := <-got:
		if el < 20*time.Millisecond || el > time.Minute {
			t.Errorf("recorded elapsed = %v, want between 20ms (the handler's sleep) and 1m", el)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the Context was not released")
	}
}
