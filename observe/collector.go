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

// EngineMetrics is a type alias for [engine.EngineMetrics], re-exported here so
// users of the observe package do not need to import engine directly.
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
	// CPUUtilization is the system CPU utilization as a fraction [0.0, 1.0].
	// Returns -1 if no CPU monitor is configured or sampling failed.
	CPUUtilization float64
}

// CPUMonitor provides CPU utilization sampling. Implementations are
// platform-specific: Linux uses /proc/stat, other platforms use
// runtime/metrics. Use [NewCPUMonitor] to create the appropriate
// implementation for the current platform.
//
// Call Close when the monitor is no longer needed to release resources
// (e.g., the /proc/stat file descriptor on Linux).
type CPUMonitor interface {
	// Sample returns the current CPU utilization as a fraction in [0.0, 1.0].
	Sample() (float64, error)
	// Close releases any resources held by the monitor.
	Close() error
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
	cpuMon          CPUMonitor
}

// NewCollector creates a new Collector with zeroed counters. The server creates
// one automatically unless Config.DisableMetrics is true.
func NewCollector() *Collector {
	return &Collector{}
}

// SetEngineMetricsFn registers a function that returns current engine metrics.
func (c *Collector) SetEngineMetricsFn(fn func() EngineMetrics) {
	c.mu.Lock()
	c.engineMetricsFn = fn
	c.mu.Unlock()
}

// SetCPUMonitor registers a CPU utilization monitor. When set, Snapshot()
// includes a CPUUtilization field sampled on each call (~every 15s is typical).
func (c *Collector) SetCPUMonitor(m CPUMonitor) {
	c.mu.Lock()
	c.cpuMon = m
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
	lo, hi := 0, bucketCount-1
	for lo < hi {
		mid := (lo + hi) / 2
		if ns <= defaultBucketBoundsNS[mid] {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	c.latencyBuckets[lo].Add(1)
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
		CPUUtilization: -1,
	}
	c.mu.Lock()
	fn := c.engineMetricsFn
	mon := c.cpuMon
	c.mu.Unlock()
	if fn != nil {
		snap.EngineMetrics = fn()
		snap.ActiveConns = snap.EngineMetrics.ActiveConnections
	}
	if mon != nil {
		if util, err := mon.Sample(); err == nil {
			snap.CPUUtilization = util
		}
	}
	return snap
}
