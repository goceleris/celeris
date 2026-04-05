// Package observe provides lightweight, lock-free metrics collection for
// celeris servers.
//
// Use [Collector.Snapshot] to retrieve a point-in-time [Snapshot] containing
// request counts, error rates, latency histogram, active connections, and
// engine-level metrics. All recording methods are safe for concurrent use.
//
// For Prometheus and debug endpoint integration, see the
// middleware/metrics package.
package observe
