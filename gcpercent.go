package celeris

import (
	"runtime/debug"
	"runtime/metrics"
	"sync"
)

// gcPercentState remembers a process GC target change made by one Server
// through Config.GCPercent (celeris#756), so that Shutdown can undo it.
//
// The zero value is "nothing changed". SetGCPercent is process-global, so the
// state never assumes it is the only writer: restore puts the previous value
// back only when the process value is still the one this server set.
type gcPercentState struct {
	mu      sync.Mutex
	changed bool
	prev    int // the target before apply
	applied int // the target apply set
}

// gcPercentNow reads the process GC target without writing it, from the
// runtime/metrics sample /gc/gogc:percent (the target in force, -1 when the
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
// start path, never from init. A second call without a restore in between is
// a no-op, so the first previous value is the one kept.
func (g *gcPercentState) apply(pct int) {
	if pct <= 0 {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.changed {
		return
	}
	g.prev = debug.SetGCPercent(pct)
	g.applied = pct
	g.changed = true
}

// restore puts the previous GC target back if this server changed it and the
// process value is still the one it set. Another writer's value (a second
// server, or any code that called SetGCPercent since) is left in force: the
// last writer wins. The check reads the target first and writes only when it
// is still ours, so a foreign value is never even touched; a writer that
// lands between the read and the write can still be overwritten, which is the
// documented last-writer-wins window. Idempotent, and a no-op when apply
// changed nothing.
func (g *gcPercentState) restore() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.changed {
		return
	}
	g.changed = false
	if cur, ok := gcPercentNow(); ok {
		if cur == g.applied {
			debug.SetGCPercent(g.prev)
		}
		return
	}
	// No metric (a runtime without the sample): the only way to read the
	// target is to write it. Set the previous value and, if the replaced
	// value was not ours, put that value back.
	if cur := debug.SetGCPercent(g.prev); cur != g.applied {
		debug.SetGCPercent(cur)
	}
}
