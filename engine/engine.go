package engine

import (
	"context"
	"time"
)

// Engine is the interface that all engine implementations must satisfy.
type Engine interface {
	Listen(ctx context.Context) error
	Shutdown(ctx context.Context) error
	Metrics() EngineMetrics
	Type() EngineType
}

// EngineMetrics is a snapshot of engine performance counters.
// Each engine implementation maintains internal atomic counters and
// populates a snapshot on Metrics() calls.
type EngineMetrics struct { //nolint:revive // user-approved name
	RequestCount      uint64
	ActiveConnections int64
	ErrorCount        uint64
	Throughput        float64
	LatencyP50        time.Duration
	LatencyP99        time.Duration
	LatencyP999       time.Duration
}
