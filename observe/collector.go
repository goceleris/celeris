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
//
// Snapshot embeds validationFields, which is defined in
// collector_validation.go under -tags=validation to carry the
// ValidationCounters field, and in collector_default.go as an empty
// struct so production binaries don't pay for the extra word. Callers
// running under -tags=validation reach the counters as
// snapshot.ValidationCounters; production callers cannot reference
// the field name at all (compile error), which is the explicit
// contract — see validation/doc.go.
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

	// validationFields is empty in production builds and carries the
	// ValidationCounters field under -tags=validation. The unused-linter
	// doesn't track embedding-as-use, so suppress its false positive.
	//nolint:unused // build-tag symmetry; non-empty under -tags=validation
	validationFields
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

// shardCount is the number of stripes for per-worker request/latency
// counters. Power of two so RecordRequestSharded can mask the worker ID
// instead of dividing. 64 is enough for current msr1 (12 cores) with
// headroom and matches typical NUMA / socket counts on bigger boxes.
const shardCount = 64
const shardMask = shardCount - 1

// shard is a cache-line-padded counter set incremented on a single
// (worker → shard) goroutine. Padding prevents MESI ping-pong between
// shards on adjacent cache lines under heavy concurrent updates.
type shard struct {
	requests atomic.Uint64
	errors   atomic.Uint64
	buckets  [bucketCount]atomic.Uint64
	// Pad to >=128 bytes so two shards never share an L1 cache line on
	// any current ARM64 / AMD64 CPU (line size 64 today, 128 on Apple
	// Silicon performance cores).
	_ [64]byte // cache-line padding; never read, kept for layout
}

// Collector aggregates request metrics using lock-free counters.
// A Collector is safe for concurrent use by multiple goroutines. Hot
// counters are striped across shardCount shards keyed by worker ID
// (set on the Context by the engine) so concurrent RecordRequest calls
// from different workers don't contend on a single cache line.
type Collector struct {
	shards          [shardCount]shard
	engineSwitches  atomic.Uint64
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
//
// Routes to shard 0 — callers who can supply a worker ID (engine handlers
// via Context.WorkerID) should prefer RecordRequestSharded to avoid the
// cross-worker cache-line contention on shard 0.
func (c *Collector) RecordRequest(duration time.Duration, status int) {
	c.RecordRequestSharded(0, duration, status)
}

// RecordRequestSharded is the worker-aware variant: caller passes a worker
// ID (or any goroutine-stable value) so the increment lands on a per-worker
// shard. Eliminates the cross-core MESI ping-pong on a single counter.
func (c *Collector) RecordRequestSharded(workerID uint32, duration time.Duration, status int) {
	s := &c.shards[workerID&shardMask]
	s.requests.Add(1)
	if status >= 500 {
		s.errors.Add(1)
	}
	// Linear scan beats binary search at this bucket count (10) when the
	// distribution is skewed toward the low end — production HTTP request
	// latencies overwhelmingly land in the first 1-2 buckets, so a linear
	// walk hits the answer in 1-2 comparisons vs binary's 3-4 (log2(10)).
	ns := duration.Nanoseconds()
	idx := bucketCount - 1
	for i := 0; i < bucketCount-1; i++ {
		if ns <= defaultBucketBoundsNS[i] {
			idx = i
			break
		}
	}
	s.buckets[idx].Add(1)
}

// RecordError increments the error counter.
//
// Note: RecordRequest automatically counts responses with status >= 500 as errors.
// Use RecordError only for errors that do not result in an HTTP response
// (e.g., connection-level failures). Lands on shard 0.
func (c *Collector) RecordError() {
	c.shards[0].errors.Add(1)
}

// RecordSwitch increments the engine switch counter.
func (c *Collector) RecordSwitch() {
	c.engineSwitches.Add(1)
}

// Snapshot returns a point-in-time copy of all collected metrics.
func (c *Collector) Snapshot() Snapshot {
	buckets := make([]uint64, bucketCount)
	var requestsTotal, errorsTotal uint64
	for i := range c.shards {
		s := &c.shards[i]
		requestsTotal += s.requests.Load()
		errorsTotal += s.errors.Load()
		for j := range s.buckets {
			buckets[j] += s.buckets[j].Load()
		}
	}
	bounds := make([]float64, len(defaultBucketBounds))
	copy(bounds, defaultBucketBounds)

	snap := Snapshot{
		RequestsTotal:  requestsTotal,
		ErrorsTotal:    errorsTotal,
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
	// fillValidation is a no-op in production builds (see
	// collector_default.go); under -tags=validation it copies the
	// atomic counters from the validation package into the
	// snapshot's embedded validationFields.
	snap.fillValidation()
	return snap
}
