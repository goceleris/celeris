//go:build linux

package iouring

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/internal/resource"
)

var asyncHeaderDeadlineSink int64

// BenchmarkAsyncFeedHeaderDeadline is what the header-timer check costs on
// every recv handleRecv feeds to a promoted async conn's dispatch goroutine
// (and once per promotion), in the steady state: a timer is configured, none
// is in flight, and the deadline is clear because the last request's headers
// were parsed. impl=unlocked is the check main made, an unlocked read of
// cs.h1State (celeris#722's race); impl=trylock is asyncHeaderDeadline, the
// same read under an uncontended detachMu.TryLock. Compare with
// benchstat -col /impl.
func BenchmarkAsyncFeedHeaderDeadline(b *testing.B) {
	w := &Worker{cfg: resource.Config{ReadHeaderTimeout: 10 * time.Second}}
	cs := &connState{detachMu: &sync.Mutex{}, h1State: conn.NewH1State()}
	b.Run("impl=unlocked", func(b *testing.B) {
		var n int64
		for b.Loop() {
			if cs.h1State != nil && cs.h1State.HeaderDeadlineNs.Load() > 0 && !cs.headerTimerArmed {
				n++
			}
		}
		asyncHeaderDeadlineSink = n
	})
	b.Run("impl=trylock", func(b *testing.B) {
		var n int64
		for b.Loop() {
			n += w.asyncHeaderDeadline(cs)
		}
		asyncHeaderDeadlineSink = n
	})
}

// startDetachMuContender runs a goroutine that locks and unlocks mu in a loop,
// the dispatch goroutine's side of cs.detachMu, and returns the function that
// stops it and waits for it. It is running before startDetachMuContender
// returns.
func startDetachMuContender(mu *sync.Mutex) (stop func()) {
	var halt atomic.Bool
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		for !halt.Load() {
			mu.Lock()
			mu.Unlock() // the empty critical section is the contention (SA2001 is excluded for tests)
		}
	}()
	<-started
	return func() { halt.Store(true); <-done }
}

// BenchmarkAsyncFeedHeaderDeadlineContended is BenchmarkAsyncFeedHeaderDeadline
// with the dispatch goroutine's side of cs.detachMu running: a second
// goroutine locks and unlocks it in a loop for the whole measurement, so the
// TryLock of impl=trylock is measured under cross-core contention instead of
// uncontended (celeris#892 item 6). Both impls run under the same contender,
// so benchstat -col /impl compares like with like; impl=unlocked never touches
// the mutex and shows what the contender costs the loop on its own. Run it
// with -cpu 1,2,4: at 1 the contender only time-slices with the loop, at 2 and
// 4 it runs on another core.
//
// A TryLock that misses makes asyncHeaderDeadline return 0 at once, which is
// cheaper than a hit, so ns/op alone flatters a heavily contended lock. The
// trylock-miss-share metric is the share of calls that missed, sampled after the
// timed loop, under the same contender, with a deadline set so that a hit and
// a miss are told apart by the result.
func BenchmarkAsyncFeedHeaderDeadlineContended(b *testing.B) {
	newCS := func() (*Worker, *connState) {
		w := &Worker{cfg: resource.Config{ReadHeaderTimeout: 10 * time.Second}}
		return w, &connState{detachMu: &sync.Mutex{}, h1State: conn.NewH1State()}
	}
	b.Run("impl=unlocked", func(b *testing.B) {
		_, cs := newCS()
		stop := startDetachMuContender(cs.detachMu)
		defer stop()
		var n int64
		for b.Loop() {
			if cs.h1State != nil && cs.h1State.HeaderDeadlineNs.Load() > 0 && !cs.headerTimerArmed {
				n++
			}
		}
		asyncHeaderDeadlineSink = n
	})
	b.Run("impl=trylock", func(b *testing.B) {
		w, cs := newCS()
		stop := startDetachMuContender(cs.detachMu)
		defer stop()
		var n int64
		for b.Loop() {
			n += w.asyncHeaderDeadline(cs)
		}
		asyncHeaderDeadlineSink = n
		// The timed loop is over; the contender is still running.
		const samples = 1 << 16
		cs.h1State.HeaderDeadlineNs.Store(1)
		misses := 0
		for range samples {
			if w.asyncHeaderDeadline(cs) == 0 {
				misses++
			}
		}
		b.ReportMetric(float64(misses)/samples, "trylock-miss-share")
	})
}
