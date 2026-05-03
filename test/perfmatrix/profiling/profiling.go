// Package profiling captures per-cell pprof artifacts when the
// orchestrator is run with -profile. CPU profiles are captured
// concurrently with the bench window; heap and goroutine profiles are
// post-bench snapshots.
package profiling

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Profileable is satisfied by servers.Server implementations that expose
// a net/http/pprof endpoint. Orchestrator type-asserts against this
// interface: if a server does not satisfy it, profile capture is
// silently skipped for that cell.
type Profileable interface {
	ProfilePort() int // 0 when this server has no pprof endpoint
}

// Config is the per-cell capture configuration.
type Config struct {
	// TargetURL is the base URL the pprof endpoints hang off, e.g.
	// "http://127.0.0.1:47351". Individual capture functions append
	// "/debug/pprof/<kind>..." paths.
	TargetURL string
	// OutDir is the directory the pprof files land in. Created if it
	// does not exist.
	OutDir string
	// CPUDur is the CPU profile duration. Typically equals the bench
	// window so the samples align with the workload.
	CPUDur time.Duration
	// CPUFile, HeapFile, GoroutineFile override the default file names
	// (cpu.pprof, heap.pprof, goroutine.pprof). Empty = default.
	CPUFile       string
	HeapFile      string
	GoroutineFile string
}

// Capture is the set of pprof artifacts collected for one cell. Empty
// strings mean the corresponding capture was skipped or failed.
type Capture struct {
	CPUPath       string
	HeapPath      string
	GoroutinePath string
}

// Target is the subject of a profile capture, retained for backward
// compatibility with the scaffold callers.
type Target struct {
	Addr         string
	OutDir       string
	ScenarioName string
	ServerName   string
	RunIdx       int
	CPUDur       time.Duration
}

// ErrNoTarget is returned when Capture* functions are invoked without a
// TargetURL. Callers should branch on this to decide whether to log or
// proceed silently (scaffold-era behaviour: silent skip).
var ErrNoTarget = errors.New("perfmatrix/profiling: no target URL")

// CaptureCPU downloads /debug/pprof/profile?seconds=<CPUDur> and writes
// it to <OutDir>/<CPUFile>. Returns the written path. The HTTP request
// blocks for CPUDur seconds so this function is typically called in its
// own goroutine during the bench window.
func CaptureCPU(ctx context.Context, cfg Config) (string, error) {
	if cfg.TargetURL == "" {
		return "", ErrNoTarget
	}
	if cfg.CPUDur <= 0 {
		cfg.CPUDur = 30 * time.Second
	}
	if cfg.CPUFile == "" {
		cfg.CPUFile = "cpu.pprof"
	}
	// The HTTP request itself needs a timeout longer than CPUDur since
	// the server blocks for CPUDur before returning the profile bytes.
	// Add a generous buffer (+30 s) for network + framing overhead.
	rctx, cancel := context.WithTimeout(ctx, cfg.CPUDur+30*time.Second)
	defer cancel()
	url := fmt.Sprintf("%s/debug/pprof/profile?seconds=%d",
		cfg.TargetURL, int(cfg.CPUDur.Seconds()))
	return downloadTo(rctx, url, cfg.OutDir, cfg.CPUFile)
}

// CaptureHeap downloads /debug/pprof/heap (a runtime snapshot) into
// <OutDir>/<HeapFile>. Intended to be called immediately after the
// bench window completes.
func CaptureHeap(ctx context.Context, cfg Config) (string, error) {
	if cfg.TargetURL == "" {
		return "", ErrNoTarget
	}
	if cfg.HeapFile == "" {
		cfg.HeapFile = "heap.pprof"
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return downloadTo(rctx, cfg.TargetURL+"/debug/pprof/heap", cfg.OutDir, cfg.HeapFile)
}

// CaptureGoroutine downloads /debug/pprof/goroutine into
// <OutDir>/<GoroutineFile>.
func CaptureGoroutine(ctx context.Context, cfg Config) (string, error) {
	if cfg.TargetURL == "" {
		return "", ErrNoTarget
	}
	if cfg.GoroutineFile == "" {
		cfg.GoroutineFile = "goroutine.pprof"
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return downloadTo(rctx, cfg.TargetURL+"/debug/pprof/goroutine", cfg.OutDir, cfg.GoroutineFile)
}

// CaptureAll is the scaffold-compatible one-shot wrapper: it captures
// CPU + heap + goroutine in sequence (CPU first, blocking for
// Target.CPUDur, then the post-bench snapshots). New callers should
// prefer the individual Capture* functions so CPU capture can run
// concurrent with the bench window.
func CaptureAll(ctx context.Context, t Target) (Capture, error) {
	cfg := Config{
		TargetURL: "http://" + t.Addr,
		OutDir:    t.OutDir,
		CPUDur:    t.CPUDur,
	}
	if t.ScenarioName != "" && t.ServerName != "" {
		prefix := fmt.Sprintf("%s-%s-run%d", t.ScenarioName, t.ServerName, t.RunIdx)
		cfg.CPUFile = prefix + "-cpu.pprof"
		cfg.HeapFile = prefix + "-heap.pprof"
		cfg.GoroutineFile = prefix + "-goroutine.pprof"
	}
	var out Capture
	cpuPath, err := CaptureCPU(ctx, cfg)
	if err != nil {
		return out, err
	}
	out.CPUPath = cpuPath
	heapPath, err := CaptureHeap(ctx, cfg)
	if err != nil {
		return out, err
	}
	out.HeapPath = heapPath
	grPath, err := CaptureGoroutine(ctx, cfg)
	if err != nil {
		return out, err
	}
	out.GoroutinePath = grPath
	return out, nil
}

// downloadTo fetches url and writes the response body to
// filepath.Join(dir, name). Creates dir if missing. Returns the
// absolute path (after any cleaning) of the written file.
func downloadTo(ctx context.Context, url, dir, name string) (string, error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("pprof GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Drain so keep-alive can reuse the conn.
		_, _ = io.Copy(io.Discard, resp.Body)
		return "", fmt.Errorf("pprof GET %s: status %d", url, resp.StatusCode)
	}
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}
