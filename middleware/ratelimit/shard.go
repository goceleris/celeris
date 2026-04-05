package ratelimit

import (
	"context"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/goceleris/celeris/middleware/internal/fnv1a"
)

type shardedLimiter struct {
	shards []shard
	mask   uint64
	rps    float64
	burst  int
}

type shard struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens   float64
	lastFill int64 // UnixNano
}

// slidingWindowLimiter uses a sliding window counter algorithm.
// It tracks the previous window count + current window count, weighted
// by elapsed fraction of the current window.
//
// Caveat: undo under concurrent load near a window boundary may restore
// a count into a window that has already advanced, slightly skewing the
// weighted estimate. This is inherent to the sliding-window-counter
// algorithm and is acceptable for rate limiting purposes.
type slidingWindowLimiter struct {
	shards     []swShard
	mask       uint64
	limit      int
	windowNano int64
}

type swShard struct {
	mu      sync.Mutex
	windows map[string]*swBucket
}

type swBucket struct {
	prevCount   int
	currCount   int
	windowStart int64 // UnixNano of current window start
}

func newSlidingWindowLimiter(ctx context.Context, shardCount int, rps float64, limit int, cleanupInterval time.Duration) *slidingWindowLimiter {
	n := fnv1a.NextPow2(shardCount)
	windowNano := int64(float64(limit) / rps * float64(time.Second))
	if windowNano <= 0 {
		windowNano = int64(time.Second)
	}
	l := &slidingWindowLimiter{
		shards:     make([]swShard, n),
		mask:       uint64(n - 1),
		limit:      limit,
		windowNano: windowNano,
	}
	for i := range l.shards {
		l.shards[i].windows = make(map[string]*swBucket)
	}

	if ctx == nil {
		ctx = context.Background()
	}
	go l.cleanup(ctx, cleanupInterval)

	return l
}

// allow checks if a request for the given key is allowed under the sliding window.
func (l *slidingWindowLimiter) allow(key string, now int64) (bool, int, int64) {
	idx := fnv1a.Hash(key) & l.mask
	s := &l.shards[idx]
	s.mu.Lock()

	w, ok := s.windows[key]
	if !ok {
		w = &swBucket{
			prevCount:   0,
			currCount:   0,
			windowStart: now,
		}
		s.windows[key] = w
	}

	l.advanceWindow(w, now)

	elapsed := now - w.windowStart
	if elapsed < 0 {
		elapsed = 0
	}
	fraction := float64(elapsed) / float64(l.windowNano)
	if fraction > 1 {
		fraction = 1
	}

	// Weighted count: previous window contribution + current window count.
	// Use math.Floor for consistent truncation (issue #13).
	weight := float64(w.prevCount)*(1-fraction) + float64(w.currCount)
	remaining := l.limit - int(math.Floor(weight)) - 1

	if weight+1 > float64(l.limit) {
		resetNano := w.windowStart + l.windowNano
		if remaining < 0 {
			remaining = 0
		}
		s.mu.Unlock()
		// Return remaining clamped to 0, not remaining+1 (issue #4).
		return false, remaining, resetNano
	}

	w.currCount++
	if remaining < 0 {
		remaining = 0
	}
	resetNano := w.windowStart + l.windowNano
	s.mu.Unlock()
	return true, remaining, resetNano
}

// undo removes one count from the current window, clamped at zero.
func (l *slidingWindowLimiter) undo(key string) {
	idx := fnv1a.Hash(key) & l.mask
	s := &l.shards[idx]
	s.mu.Lock()
	if w, ok := s.windows[key]; ok && w.currCount > 0 {
		w.currCount--
	}
	s.mu.Unlock()
}

func (l *slidingWindowLimiter) advanceWindow(w *swBucket, now int64) {
	if now < w.windowStart {
		return
	}
	elapsed := now - w.windowStart
	if elapsed >= 2*l.windowNano {
		// Two or more full windows elapsed: both counts are stale (issue #5).
		w.prevCount = 0
		w.currCount = 0
		w.windowStart = now
	} else if elapsed >= l.windowNano {
		w.prevCount = w.currCount
		w.currCount = 0
		w.windowStart = w.windowStart + l.windowNano
	}
}

func (l *slidingWindowLimiter) cleanup(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	shardIdx := 0
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			// Process min(4, len(shards)) per tick instead of 1 (issue #8).
			n := 4
			if n > len(l.shards) {
				n = len(l.shards)
			}
			expiry := now.UnixNano() - 2*l.windowNano
			for range n {
				s := &l.shards[shardIdx%len(l.shards)]
				// Wrap shardIdx to avoid overflow on 32-bit (issue #12).
				shardIdx = (shardIdx + 1) % len(l.shards)
				s.mu.Lock()
				for k, w := range s.windows {
					if w.windowStart < expiry && w.currCount == 0 && w.prevCount == 0 {
						delete(s.windows, k)
					}
				}
				s.mu.Unlock()
			}
		}
	}
}

func newShardedLimiter(ctx context.Context, shardCount int, rps float64, burst int, cleanupInterval time.Duration) *shardedLimiter {
	n := fnv1a.NextPow2(shardCount)
	l := &shardedLimiter{
		shards: make([]shard, n),
		mask:   uint64(n - 1),
		rps:    rps,
		burst:  burst,
	}
	for i := range l.shards {
		l.shards[i].buckets = make(map[string]*bucket)
	}

	if ctx == nil {
		ctx = context.Background()
	}
	go l.cleanup(ctx, cleanupInterval)

	return l
}

// allow checks if a request for the given key is allowed.
// Returns (allowed bool, remaining int, resetUnixNano int64).
func (l *shardedLimiter) allow(key string, now int64) (bool, int, int64) {
	idx := fnv1a.Hash(key) & l.mask
	s := &l.shards[idx]
	s.mu.Lock()

	b, ok := s.buckets[key]
	if !ok {
		b = &bucket{
			tokens:   float64(l.burst) - 1,
			lastFill: now,
		}
		s.buckets[key] = b
		s.mu.Unlock()
		reset := now + int64(float64(time.Second)/l.rps)
		return true, l.burst - 1, reset
	}

	// Refill tokens. Clamp elapsed to zero to handle clock step-backs (NTP).
	elapsed := float64(now-b.lastFill) / float64(time.Second)
	if elapsed < 0 {
		elapsed = 0
	}
	b.tokens += elapsed * l.rps
	if b.tokens > float64(l.burst) {
		b.tokens = float64(l.burst)
	}
	b.lastFill = now

	if b.tokens < 1 {
		remaining := 0
		wait := (1 - b.tokens) / l.rps
		reset := now + int64(wait*float64(time.Second))
		s.mu.Unlock()
		return false, remaining, reset
	}

	b.tokens--
	remaining := int(b.tokens)
	reset := now + int64(float64(time.Second)/l.rps)
	s.mu.Unlock()
	return true, remaining, reset
}

// undo adds a token back to the bucket for the given key, capped at burst.
func (l *shardedLimiter) undo(key string) {
	idx := fnv1a.Hash(key) & l.mask
	s := &l.shards[idx]
	s.mu.Lock()
	if b, ok := s.buckets[key]; ok {
		b.tokens++
		if b.tokens > float64(l.burst) {
			b.tokens = float64(l.burst)
		}
	}
	s.mu.Unlock()
}

func (l *shardedLimiter) cleanup(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	shardIdx := 0
	// Derive expiry from rate: max(10s, 2 * burst/rps) (issue #6).
	expiryDuration := time.Duration(float64(l.burst)/l.rps*2) * time.Second
	if expiryDuration < 10*time.Second {
		expiryDuration = 10 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			// Process min(4, len(shards)) per tick instead of 1 (issue #8).
			n := 4
			if n > len(l.shards) {
				n = len(l.shards)
			}
			expiry := now.UnixNano() - int64(expiryDuration)
			for range n {
				s := &l.shards[shardIdx%len(l.shards)]
				// Wrap shardIdx to avoid overflow on 32-bit (issue #12).
				shardIdx = (shardIdx + 1) % len(l.shards)
				s.mu.Lock()
				for k, b := range s.buckets {
					if b.lastFill < expiry && b.tokens >= float64(l.burst) {
						delete(s.buckets, k)
					}
				}
				s.mu.Unlock()
			}
		}
	}
}

// smallInts caches string representations of integers 0-999.
// Covers >99% of rate limit remaining/reset values without allocation.
var smallInts [1000]string

func init() {
	for i := range smallInts {
		smallInts[i] = strconv.Itoa(i)
	}
}

func formatInt(n int) string {
	if n >= 0 && n < len(smallInts) {
		return smallInts[n]
	}
	return strconv.Itoa(n)
}

// formatResetSeconds returns the ceiling-rounded number of whole seconds
// between nowNano and resetNano, clamped to a minimum of 0. When the
// reset time is in the past, 0 is returned.
func formatResetSeconds(resetNano, nowNano int64) string {
	secs := (resetNano - nowNano + int64(time.Second) - 1) / int64(time.Second)
	if secs < 0 {
		secs = 0
	}
	if secs < int64(len(smallInts)) {
		return smallInts[secs]
	}
	return strconv.FormatInt(secs, 10)
}
