package observe

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris/engine"
)

const bucketCount = 10

var defaultBucketBounds = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 5,
}

var defaultBucketBoundsNS [bucketCount]int64

func init() {
	for i, bound := range defaultBucketBounds {
		defaultBucketBoundsNS[i] = int64(bound * 1e9)
	}
}

// EngineMetrics is a point-in-time snapshot of engine-level performance counters.
type EngineMetrics = engine.EngineMetrics

// Snapshot is a point-in-time copy of all collected metrics. All fields are
// read-only values captured at the moment Collector.Snapshot was called.
type Snapshot struct {
	// RequestsTotal is the cumulative number of handled requests.
	RequestsTotal uint64
	// ErrorsTotal is the cumulative number of requests that returned HTTP 5xx.
	ErrorsTotal uint64
	// ActiveConns is the number of currently open connections.
	ActiveConns int64
	// EngineSwitches counts how many times the adaptive engine changed strategies.
	EngineSwitches uint64
	// LatencyBuckets holds request counts per latency histogram bucket.
	LatencyBuckets []uint64
	// BucketBounds are the upper-bound thresholds (in seconds) for each bucket.
	BucketBounds []float64
	// EngineMetrics contains the underlying engine's own performance counters.
	EngineMetrics EngineMetrics
}

// Collector aggregates request metrics using lock-free counters.
// A Collector is safe for concurrent use by multiple goroutines.
type Collector struct {
	requestsTotal   atomic.Uint64
	errorsTotal     atomic.Uint64
	engineSwitches  atomic.Uint64
	latencyBuckets  [bucketCount]atomic.Uint64
	mu              sync.Mutex
	engineMetricsFn func() EngineMetrics
}

// NewCollector creates a new Collector.
func NewCollector() *Collector {
	return &Collector{}
}

// SetEngineMetricsFn registers a function that returns current engine metrics.
func (c *Collector) SetEngineMetricsFn(fn func() EngineMetrics) {
	c.mu.Lock()
	c.engineMetricsFn = fn
	c.mu.Unlock()
}

// RecordRequest increments the request counter and records the latency in the
// appropriate histogram bucket. Status codes >= 500 also increment the error counter.
func (c *Collector) RecordRequest(duration time.Duration, status int) {
	c.requestsTotal.Add(1)
	if status >= 500 {
		c.errorsTotal.Add(1)
	}
	ns := duration.Nanoseconds()
	for i, bound := range defaultBucketBoundsNS {
		if ns <= bound {
			c.latencyBuckets[i].Add(1)
			return
		}
	}
	c.latencyBuckets[bucketCount-1].Add(1)
}

// RecordError increments the error counter.
//
// Note: RecordRequest automatically counts responses with status >= 500 as errors.
// Use RecordError only for errors that do not result in an HTTP response
// (e.g., connection-level failures).
func (c *Collector) RecordError() {
	c.errorsTotal.Add(1)
}

// RecordSwitch increments the engine switch counter.
func (c *Collector) RecordSwitch() {
	c.engineSwitches.Add(1)
}

// Snapshot returns a point-in-time copy of all collected metrics.
func (c *Collector) Snapshot() Snapshot {
	buckets := make([]uint64, bucketCount)
	for i := range c.latencyBuckets {
		buckets[i] = c.latencyBuckets[i].Load()
	}
	bounds := make([]float64, len(defaultBucketBounds))
	copy(bounds, defaultBucketBounds)

	snap := Snapshot{
		RequestsTotal:  c.requestsTotal.Load(),
		ErrorsTotal:    c.errorsTotal.Load(),
		EngineSwitches: c.engineSwitches.Load(),
		LatencyBuckets: buckets,
		BucketBounds:   bounds,
	}
	c.mu.Lock()
	fn := c.engineMetricsFn
	c.mu.Unlock()
	if fn != nil {
		snap.EngineMetrics = fn()
		snap.ActiveConns = snap.EngineMetrics.ActiveConnections
	}
	return snap
}
