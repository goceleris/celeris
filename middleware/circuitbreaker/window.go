package circuitbreaker

import (
	"sync/atomic"
	"time"
)

const numBuckets = 10

type bucket struct {
	successes atomic.Int64
	failures  atomic.Int64
	epoch     atomic.Int64 // start time of this bucket in nanoseconds
}

type slidingWindow struct {
	buckets    [numBuckets]bucket
	bucketSize int64 // nanoseconds per bucket
	windowSize int64 // nanoseconds for the full window
}

func newSlidingWindow(windowSize time.Duration) *slidingWindow {
	w := &slidingWindow{
		bucketSize: int64(windowSize) / numBuckets,
		windowSize: int64(windowSize),
	}
	return w
}

func (w *slidingWindow) currentBucket(now int64) *bucket {
	idx := (now / w.bucketSize) % numBuckets
	b := &w.buckets[idx]
	epoch := (now / w.bucketSize) * w.bucketSize
	if old := b.epoch.Load(); old != epoch {
		if b.epoch.CompareAndSwap(old, epoch) {
			b.successes.Store(0)
			b.failures.Store(0)
		}
	}
	return b
}

func (w *slidingWindow) recordSuccess() {
	now := time.Now().UnixNano()
	b := w.currentBucket(now)
	b.successes.Add(1)
}

func (w *slidingWindow) recordFailure() {
	now := time.Now().UnixNano()
	b := w.currentBucket(now)
	b.failures.Add(1)
}

func (w *slidingWindow) counts() (total, failures int64) {
	now := time.Now().UnixNano()
	cutoff := now - w.windowSize
	for i := range numBuckets {
		b := &w.buckets[i]
		if b.epoch.Load() >= cutoff {
			s := b.successes.Load()
			f := b.failures.Load()
			total += s + f
			failures += f
		}
	}
	return total, failures
}

func (w *slidingWindow) reset() {
	for i := range numBuckets {
		w.buckets[i].successes.Store(0)
		w.buckets[i].failures.Store(0)
		w.buckets[i].epoch.Store(0)
	}
}
