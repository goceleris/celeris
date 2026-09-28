//go:build linux

package iouring

import (
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris/internal/conn"
	"github.com/goceleris/celeris/resource"
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
