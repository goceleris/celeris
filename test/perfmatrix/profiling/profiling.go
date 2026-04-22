// Package profiling captures per-cell pprof artifacts when the
// orchestrator is run with -profile. Wave-3 implements the real capture
// loop; this scaffold pins the contract.
package profiling

import (
	"context"
	"errors"
)

// ErrNotImplemented is returned by every scaffold stub here.
var ErrNotImplemented = errors.New("perfmatrix/profiling: not yet implemented")

// Capture is the set of pprof artifacts collected for one cell.
type Capture struct {
	CPUPath       string
	HeapPath      string
	GoroutinePath string
	BlockPath     string
	MutexPath     string
}

// Target is the subject of a profile capture. The server exposes a debug
// pprof endpoint or the orchestrator calls into runtime/pprof directly.
type Target struct {
	// Addr is the host:port of a running pprof debug endpoint. Empty
	// means capture the orchestrator process itself instead.
	Addr string
	// OutDir is the directory the captured files land in. One file per
	// profile kind, named "<scenario>-<server>-run<RunIdx>-<kind>.pprof".
	OutDir string
	// ScenarioName, ServerName and RunIdx are stamped into the filenames
	// so a capture is traceable back to its cell.
	ScenarioName string
	ServerName   string
	RunIdx       int
}

// CaptureAll collects every profile kind for the supplied target. The
// returned Capture lists the paths actually written (empty string if a
// given kind was skipped / unsupported).
func CaptureAll(ctx context.Context, t Target) (Capture, error) {
	_ = ctx
	_ = t
	return Capture{}, ErrNotImplemented
}
