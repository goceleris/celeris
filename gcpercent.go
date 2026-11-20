package celeris

import (
	"runtime/debug"
	"runtime/metrics"
	"sync"
)

// gcCoord is the process-wide record of Config.GCPercent changes (celeris#756).
// SetGCPercent is process-global, so the servers of one process share one
// record instead of each remembering its own previous value:
//
//   - active counts the servers that applied a GCPercent and have not yet
//     released it;
//   - orig is the target in force before the FIRST of them applied (valid while
//     active > 0);
//   - last is the target celeris wrote most recently.
//
// When the last active server releases, orig is put back, but only if the
// process target still equals last: any other value was written by someone
// else and is left alone. While several servers are active, the last value
// written stays in force, whichever server stops first.
var gcCoord struct {
	mu     sync.Mutex
	active int
	orig   int
	last   int
}

// gcPercentState is one Server's share of gcCoord: whether it holds an active
// opt-in. The zero value is "nothing applied".
type gcPercentState struct {
	mu      sync.Mutex
	counted bool
}

// gcPercentNow reads the process GC target without writing it, from the
// runtime/metrics sample /gc/gogc:percent (the target in force; -1 when the
// collector is off). The second result is false if the runtime does not offer
// the sample.
func gcPercentNow() (int, bool) {
	s := []metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindUint64 {
		return 0, false
	}
	return int(s[0].Value.Uint64()), true
}

// apply sets the process GC target to pct when pct > 0. pct == 0 leaves the
// runtime untouched (the default). It is called once per server, from the
// start path, never from init; a second call without a restore in between is
// a no-op.
func (g *gcPercentState) apply(pct int) {
	if pct <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.counted {
		return
	}
	gcCoord.mu.Lock()
	defer gcCoord.mu.Unlock()
	prev := debug.SetGCPercent(pct)
	if gcCoord.active == 0 {
		gcCoord.orig = prev
	}
	gcCoord.last = pct
	gcCoord.active++
	g.counted = true
}

// restore releases this server's opt-in. The server that releases last puts
// the original target back if the process target is still the last value
// celeris wrote; a target someone else wrote is left in force, and so is the
// target when the runtime offers no way to read it without writing. It runs
// after the drain on a normal Shutdown, and after Listen returned for a Start*
// that ends without one; if the Shutdown context expires first, it runs while
// a drain may still be going and that drain finishes at the old target.
// Idempotent, and a no-op when apply changed nothing.
func (g *gcPercentState) restore() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.counted {
		return
	}
	g.counted = false
	gcCoord.mu.Lock()
	defer gcCoord.mu.Unlock()
	gcCoord.active--
	if gcCoord.active > 0 {
		return
	}
	if cur, ok := gcPercentNow(); ok && cur == gcCoord.last {
		debug.SetGCPercent(gcCoord.orig)
	}
}
