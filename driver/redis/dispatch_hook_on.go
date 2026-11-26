//go:build redis_delay_hook

package redis

import (
	"sync/atomic"
	"time"
)

// Test-only build (-tags redis_delay_hook): dispatchHook pauses one dispatch
// in every ten for dispatchDelay, between taking a request and writing the
// reply into it, the way a descheduled event-loop worker would.
var (
	dispatchDelay atomic.Int64 // nanoseconds; 0 = off
	dispatchCalls atomic.Uint64
)

func dispatchHook() {
	if d := dispatchDelay.Load(); d > 0 && dispatchCalls.Add(1)%10 == 0 {
		time.Sleep(time.Duration(d))
	}
}
