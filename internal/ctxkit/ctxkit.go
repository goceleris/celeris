// Package ctxkit provides small context helpers shared between celeris
// root-package and sub-packages that cannot import the root due to
// circular-dependency constraints.
package ctxkit

import "context"

// ReleaseContext is registered by the celeris package at init time.
// internal/conn/h1.go uses it to release cached contexts on connection close.
var ReleaseContext func(c any)

// workerIDKey is the private context key carrying the event-loop worker ID
// that accepted / is servicing the current connection. Engines set it at
// accept time via WithWorkerID; handlers read it via
// celeris.Context.WorkerID() and may forward it to a DB / cache driver
// pool via the driver's WithWorker option for per-CPU affinity.
type workerIDKey struct{}

// WithWorkerID annotates ctx with the event-loop worker ID that owns the
// connection. Engines call this at accept-time so per-request handlers
// can thread the worker affinity through driver calls without the
// engines and the celeris root package coupling directly.
//
// id is the worker's numeric ID (0..NumWorkers-1). A worker ID of -1
// means no meaningful worker identity (e.g. the std engine where Go's
// scheduler picks the goroutine).
func WithWorkerID(ctx context.Context, id int) context.Context {
	return context.WithValue(ctx, workerIDKey{}, id)
}

// WorkerIDFrom extracts the engine worker ID stored by WithWorkerID.
// Returns (-1, false) when the context carries no annotation (synthetic
// contexts, tests without a Server, etc.).
func WorkerIDFrom(ctx context.Context) (int, bool) {
	v := ctx.Value(workerIDKey{})
	if v == nil {
		return -1, false
	}
	id, ok := v.(int)
	if !ok {
		return -1, false
	}
	return id, true
}
