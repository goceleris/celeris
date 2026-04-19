package async

import (
	"context"
	"sync/atomic"
	"time"
)

// healthLoop runs in a background goroutine for pools that configure a
// positive HealthCheck interval. On each tick it iterates every worker's
// idle list, evicts expired/broken connections, and pings the remaining
// ones with a short timeout.
func (p *Pool[C]) healthLoop() {
	defer close(p.hcDone)
	ticker := time.NewTicker(p.cfg.HealthCheck)
	defer ticker.Stop()
	for {
		select {
		case <-p.hcStop:
			return
		case now := <-ticker.C:
			p.healthSweep(now)
		}
	}
}

// healthSweep does one pass over every worker's idle list. Conns that are
// expired, idle-too-long, or fail Ping are closed and removed from the
// pool. Per-worker locks are held only while mutating the idle slice.
func (p *Pool[C]) healthSweep(now time.Time) {
	for _, slot := range p.perWorker {
		slot.mu.Lock()
		if len(slot.idle) == 0 {
			slot.mu.Unlock()
			continue
		}
		// Snapshot and clear; inspect outside the lock so Ping does not
		// block Acquire.
		snapshot := make([]C, len(slot.idle))
		copy(snapshot, slot.idle)
		var zero C
		for i := range slot.idle {
			slot.idle[i] = zero
		}
		slot.idle = slot.idle[:0]
		slot.mu.Unlock()

		var survivors []C
		for _, c := range snapshot {
			if c.IsExpired(now) || c.IsIdleTooLong(now) {
				_ = c.Close()
				atomic.AddInt64(&p.openCount, -1)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), pingTimeout(p.cfg.HealthCheck))
			err := c.Ping(ctx)
			cancel()
			if err != nil {
				_ = c.Close()
				atomic.AddInt64(&p.openCount, -1)
				continue
			}
			survivors = append(survivors, c)
		}

		// Reinsert survivors, honoring MaxIdlePerWorker.
		slot.mu.Lock()
		for _, c := range survivors {
			if p.cfg.MaxIdlePerWorker > 0 && len(slot.idle) >= p.cfg.MaxIdlePerWorker {
				slot.mu.Unlock()
				_ = c.Close()
				atomic.AddInt64(&p.openCount, -1)
				slot.mu.Lock()
				continue
			}
			slot.idle = append(slot.idle, c)
		}
		slot.mu.Unlock()
	}
}

// pingTimeout derives a per-ping timeout from the health-check interval.
// We never wait longer than the interval itself to keep the sweep bounded.
func pingTimeout(interval time.Duration) time.Duration {
	if interval <= 0 {
		return time.Second
	}
	if interval > 5*time.Second {
		return 5 * time.Second
	}
	return interval
}
