package report

import "io"

// WritePprofIndex generates an index.html linking every pprof capture
// produced during a -profile run (CPU, heap, goroutine, per-cell).
// Wave-3 fills in the actual HTML template.
func WritePprofIndex(w io.Writer, captures []PprofCapture) error {
	_ = w
	_ = captures
	return ErrNotImplemented
}

// PprofCapture is one pprof artifact on disk.
type PprofCapture struct {
	ScenarioName string
	ServerName   string
	RunIdx       int
	Kind         string // "cpu", "heap", "goroutine", "block", "mutex"
	Path         string // relative to the run output directory
}
