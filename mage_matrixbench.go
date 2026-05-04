//go:build mage

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// matrixFlags collects the CLI flags common to every matrixBench target.
type matrixFlags struct {
	runs     int
	duration time.Duration
	warmup   time.Duration
	cells    string
	profile  bool
	services string
	// extraArgs are appended verbatim after the standard flags; used by
	// MatrixBenchSince to inject -out pointing at a pre-created directory.
	extraArgs []string
	// outDir, if set, overrides the default results/<ts>-<ref>/ path.
	outDir string
	// strict builds the runner (and every in-process engine / server it
	// spawns) with the Go race detector and pointer-sanity checks, runs
	// the runner with GORACE=halt_on_error=1 + GOTRACEBACK=crash, and
	// passes -fail-fast to the runner. Any data race, use-after-free,
	// or invalid pointer write aborts the matrix immediately with a
	// full crash report — the same kind of bug that otherwise sits
	// buried for hours of churn load before the consequence fires.
	strict bool
}

// MatrixBench runs the full release-gate performance matrix: all celeris
// configs × all competitors × all scenarios × 5 interleaved runs at 15s
// each. Expected runtime ~1.9 days on msr1. See test/perfmatrix/README.md.
//
// PERFMATRIX_CELLS, when set, narrows the run to a comma-separated cell
// glob (same syntax as the runner's -cells flag).
//
// Warmup is 5s: the io_uring engine's per-cell ring-setup +
// SO_REUSEPORT-rebind + 128-conn handshake takes longer than 2s to
// amortize, so a shorter warmup over-flattered the cold-start-fast
// engines (fasthttp / std-net) at celeris's expense.
func MatrixBench() error {
	return runMatrix(matrixFlags{
		runs:     5,
		duration: 15 * time.Second,
		warmup:   5 * time.Second,
		cells:    envOrDefault("PERFMATRIX_CELLS", "*"),
		profile:  false,
		services: "local",
	})
}

// MatrixBenchQuick is the dev-loop variant: 3 runs × 5s, simple static GET
// cells only, services disabled. ~1 hour on msr1, ~minutes locally.
// Override the cells filter with PERFMATRIX_CELLS for custom subsets.
func MatrixBenchQuick() error {
	return runMatrix(matrixFlags{
		runs:     3,
		duration: 5 * time.Second,
		warmup:   1 * time.Second,
		cells:    envOrDefault("PERFMATRIX_CELLS", "get-*/*"),
		profile:  false,
		services: "none",
	})
}

// MatrixBenchDrivers runs only the driver-backed cells (pg/redis/memcached/
// session), 10 runs × 10s.
func MatrixBenchDrivers() error {
	return runMatrix(matrixFlags{
		runs:     10,
		duration: 10 * time.Second,
		warmup:   2 * time.Second,
		cells:    "driver-*/*",
		profile:  false,
		services: "local",
	})
}

// MatrixBenchProfile runs the matrix with pprof capture per cell. Adds
// ~12s per cell; expect ~4 days for the full matrix. Typically invoked
// with PERFMATRIX_CELLS=... for targeted profiling.
func MatrixBenchProfile() error {
	return runMatrix(matrixFlags{
		runs:     10,
		duration: 10 * time.Second,
		warmup:   2 * time.Second,
		cells:    envOrDefault("PERFMATRIX_CELLS", "*"),
		profile:  true,
		services: "local",
	})
}

// MatrixBenchStrict runs the matrix with the Go race detector,
// pointer-sanity checks, and fail-fast enabled. Every bug that
// would otherwise take 16h of churn-load to surface (data races,
// use-after-free on pooled connState, stale iovec pointers,
// invalid unsafe.Pointer conversions, …) aborts the matrix the
// moment the detector fires, with a full stack trace.
//
// Overhead: -race slows each cell ~3-5× and costs ~5-8× memory,
// so the default is tuned down (3 runs × 5s × 1s warmup) to keep
// the strict sweep in the 4-8h range on msr1. Override via
// PERFMATRIX_RUNS / PERFMATRIX_DURATION / PERFMATRIX_WARMUP when
// a full-weight strict run is wanted.
//
// Recommended as the release-gate confidence check: once a strict
// run completes without aborting, the subsequent performance
// matrix can be trusted to measure real numbers, not to be
// hunting bugs in the background.
func MatrixBenchStrict() error {
	runs, _ := strconv.Atoi(envOrDefault("PERFMATRIX_RUNS", "3"))
	durStr := envOrDefault("PERFMATRIX_DURATION", "5s")
	warmStr := envOrDefault("PERFMATRIX_WARMUP", "1s")
	dur, _ := time.ParseDuration(durStr)
	warm, _ := time.ParseDuration(warmStr)
	// Default skip: every server that wraps golang.org/x/net/http2/h2c
	// for its auto/h2c configurations. That handler has a known
	// upstream race between hpack.Encoder.SetMaxDynamicTableSize
	// (SETTINGS path) and hpack.Encoder.WriteField (frame-write
	// goroutine), which fires reliably under the first SETTINGS +
	// first HEADERS exchange. The race is not celeris code; failing
	// the matrix on it tells us nothing about celeris. Affected
	// servers: celeris-std, stdhttp, chi, echo, gin, iris. fasthttp /
	// fiber / hertz do not use x/net/http2 and stay included.
	// Override via PERFMATRIX_CELLS to force-include.
	services := envOrDefault("PERFMATRIX_SERVICES", "local")
	defaultExcludes := "!*/celeris-std-auto*,!*/celeris-std-h2c*," +
		"!*/stdhttp-auto,!*/stdhttp-h2c," +
		"!*/chi-auto,!*/chi-h2c," +
		"!*/echo-auto,!*/echo-h2c," +
		"!*/gin-auto,!*/gin-h2c," +
		"!*/iris-auto,!*/iris-h2c"
	// When services=none (host has no Docker), additionally exclude
	// driver-backed scenarios so the runner does not attempt to bench
	// cells that would deadlock waiting for postgres/redis/memcached.
	if services == "none" {
		defaultExcludes += ",!*/driver-*"
	}
	cells := envOrDefault("PERFMATRIX_CELLS", "*,"+defaultExcludes)
	return runMatrix(matrixFlags{
		runs:     runs,
		duration: dur,
		warmup:   warm,
		cells:    cells,
		profile:  false,
		services: services,
		strict:   true,
	})
}

// MatrixBenchSince runs the matrix against HEAD and a reference ref (from
// PERFMATRIX_REF, default "main"), then diffs. Any cell where HEAD is
// more than 2% slower than the baseline triggers a non-zero exit so CI
// can gate releases on regressions.
func MatrixBenchSince() error {
	ref := envOrDefault("PERFMATRIX_REF", "main")

	ts := time.Now().UTC().Format("20060102-150405")
	refShort, _ := output("git", "rev-parse", "--short", "HEAD")
	if refShort == "" {
		refShort = "local"
	}
	rootOut := filepath.Join("results", ts+"-since-"+refShort+"-vs-"+sanitizeRefLabel(ref))
	if err := os.MkdirAll(rootOut, 0o755); err != nil {
		return err
	}

	headDir := filepath.Join(rootOut, "head")
	baseDir := filepath.Join(rootOut, "base")

	fmt.Printf("MatrixBenchSince: HEAD=%s base=%s out=%s\n", refShort, ref, rootOut)

	// 1. Run HEAD.
	fmt.Println("--- Running HEAD ---")
	if err := runMatrix(matrixFlags{
		runs:     10,
		duration: 10 * time.Second,
		warmup:   2 * time.Second,
		cells:    envOrDefault("PERFMATRIX_CELLS", "*"),
		profile:  false,
		services: "local",
		outDir:   headDir,
	}); err != nil {
		return fmt.Errorf("matrix HEAD: %w", err)
	}

	// 2. Stash anything that's uncommitted, checkout the ref, run matrix
	// again, then return to the original ref.
	originalHEAD, err := output("git", "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("capture HEAD: %w", err)
	}
	stashed := false
	if out, _ := output("git", "status", "--porcelain"); strings.TrimSpace(out) != "" {
		fmt.Println("Stashing dirty tree before checkout...")
		if err := run("git", "stash", "push", "-u", "-m", "matrixBenchSince-stash"); err != nil {
			return fmt.Errorf("git stash: %w", err)
		}
		stashed = true
	}
	defer func() {
		// Always attempt to restore the original HEAD and un-stash.
		_ = run("git", "checkout", strings.TrimSpace(originalHEAD))
		if stashed {
			_ = run("git", "stash", "pop")
		}
	}()

	fmt.Printf("--- Running base ref %s ---\n", ref)
	if err := run("git", "checkout", ref); err != nil {
		return fmt.Errorf("checkout %s: %w", ref, err)
	}
	if err := runMatrix(matrixFlags{
		runs:     10,
		duration: 10 * time.Second,
		warmup:   2 * time.Second,
		cells:    envOrDefault("PERFMATRIX_CELLS", "*"),
		profile:  false,
		services: "local",
		outDir:   baseDir,
	}); err != nil {
		return fmt.Errorf("matrix base: %w", err)
	}

	// 3. Diff the two aggregates and fail if any cell regressed >2%.
	return diffMatrixReports(baseDir, headDir, rootOut)
}

// runMatrix is the shared runner that shells out to the perfmatrix
// orchestrator binary and then generates the markdown/csv/pprof reports.
func runMatrix(fl matrixFlags) error {
	out := fl.outDir
	if out == "" {
		ts := time.Now().UTC().Format("20060102-150405")
		ref, _ := output("git", "rev-parse", "--short", "HEAD")
		if ref == "" {
			ref = "local"
		}
		out = filepath.Join("results", ts+"-matrix-"+ref)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", out, err)
	}

	args := []string{"run"}
	// Profile-guided compilation. test/perfmatrix/cmd/runner/default.pgo
	// is captured from a representative get-simple/iouring run; -pgo=auto
	// folds it into the build so the benchmarked binary inlines and
	// tunes branches around the actual hot path. -race is incompatible
	// with PGO (the race instrumentation perturbs profiles), so only
	// non-strict matrix runs benefit.
	if !fl.strict {
		if _, err := os.Stat(filepath.Join("test", "perfmatrix", "cmd", "runner", "default.pgo")); err == nil {
			args = append(args, "-pgo=auto")
		}
	}
	if fl.strict {
		// -race links the race detector into the runner AND every
		// in-process server it spawns (celeris engines, competitors),
		// so a data race anywhere in the stack aborts the process on
		// detection. checkptr=2 catches invalid uintptr↔unsafe.Pointer
		// round-trips — the bug class that silently writes into
		// reclaimed memory and crashes minutes-to-hours later.
		args = append(args, "-race", "-gcflags=all=-d=checkptr=2")
	}
	args = append(args, "./cmd/runner",
		"-runs", strconv.Itoa(fl.runs),
		"-duration", fl.duration.String(),
		"-warmup", fl.warmup.String(),
		"-services", fl.services,
	)
	if fl.cells != "" {
		args = append(args, "-cells", fl.cells)
	}
	absOut, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	args = append(args, "-out", absOut)
	if fl.profile {
		args = append(args, "-profile")
	}
	if fl.strict {
		args = append(args, "-fail-fast")
	}
	args = append(args, fl.extraArgs...)

	fmt.Printf("perfmatrix: go %s\n", strings.Join(args, " "))
	cmd := exec.Command("go", args...)
	cmd.Dir = filepath.Join("test", "perfmatrix")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if fl.strict {
		// halt_on_error=1 makes a data race terminate the process with
		// exit code 66 on detection (Go default is log-and-continue).
		// GOTRACEBACK=crash dumps every goroutine's stack on any
		// signal — the post-mortem we wished we had for #256 before
		// we could reproduce it.
		cmd.Env = append(cmd.Env,
			"GORACE=halt_on_error=1 log_path=stderr",
			"GOTRACEBACK=crash",
		)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("perfmatrix runner: %w", err)
	}

	return generateMatrixReport(absOut)
}

// envOrDefault returns the env var value when set, otherwise def.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// sanitizeRefLabel turns a git ref into a path-safe label.
func sanitizeRefLabel(ref string) string {
	r := strings.NewReplacer("/", "-", ".", "-", "+", "-", " ", "-")
	return r.Replace(ref)
}

// diffMatrixReports reads the per-cell JSON artefacts from two matrix
// runs (base, head), compares RPS medians, and writes a diff report
// alongside the two source runs. Returns a non-zero-exit-compatible
// error when any cell regresses by more than 2%.
func diffMatrixReports(baseDir, headDir, outRoot string) error {
	baseAgg, err := loadAggregate(baseDir)
	if err != nil {
		return fmt.Errorf("base aggregate: %w", err)
	}
	headAgg, err := loadAggregate(headDir)
	if err != nil {
		return fmt.Errorf("head aggregate: %w", err)
	}

	type row struct {
		cell     string
		baseRPS  float64
		headRPS  float64
		deltaPct float64
	}
	var rows []row
	for id, h := range headAgg {
		b, ok := baseAgg[id]
		if !ok {
			continue
		}
		if b.RPSMedian == 0 {
			continue
		}
		delta := (h.RPSMedian - b.RPSMedian) / b.RPSMedian * 100
		rows = append(rows, row{
			cell:     id,
			baseRPS:  b.RPSMedian,
			headRPS:  h.RPSMedian,
			deltaPct: delta,
		})
	}

	var sb strings.Builder
	sb.WriteString("# perfmatrix diff report\n\n")
	fmt.Fprintf(&sb, "base=%s  head=%s\n\n", baseDir, headDir)
	sb.WriteString("| cell | base rps | head rps | delta |\n")
	sb.WriteString("| --- | ---: | ---: | ---: |\n")

	var regressions int
	for _, r := range rows {
		flag := ""
		if r.deltaPct < -2.0 {
			flag = " ⚠"
			regressions++
		}
		fmt.Fprintf(&sb, "| %s | %.0f | %.0f | %+.2f%%%s |\n",
			r.cell, r.baseRPS, r.headRPS, r.deltaPct, flag)
	}
	fmt.Fprintf(&sb, "\nRegressions (>2%%): %d\n", regressions)

	if err := os.WriteFile(filepath.Join(outRoot, "diff.md"), []byte(sb.String()), 0o644); err != nil {
		return fmt.Errorf("write diff.md: %w", err)
	}
	fmt.Println(sb.String())

	if regressions > 0 {
		return fmt.Errorf("matrixBenchSince: %d cell(s) regressed >2%%; see %s/diff.md",
			regressions, outRoot)
	}
	return nil
}

// cellMinimal is the subset of the runner's cellResultFile we need when
// loading aggregates from disk. Keeping this independent of the runner
// package means the mage target can compile standalone.
type cellMinimal struct {
	Scenario   string  `json:"scenario"`
	Server     string  `json:"server"`
	ServerKind string  `json:"server_kind"`
	Category   string  `json:"category"`
	Result     *result `json:"result,omitempty"`
}

type result struct {
	RequestsPerSec float64    `json:"requests_per_sec"`
	ThroughputBPS  float64    `json:"throughput_bps"`
	Errors         int64      `json:"errors"`
	Latency        latencyRaw `json:"latency"`
}

type latencyRaw struct {
	P50   int64 `json:"p50"`
	P90   int64 `json:"p90"`
	P99   int64 `json:"p99"`
	P999  int64 `json:"p99_9"`
	P9999 int64 `json:"p99_99"`
	Max   int64 `json:"max"`
}

// aggregatedCell mirrors report.CellAggregate just enough for the diff
// helper. We avoid pulling the perfmatrix module in as a dependency of
// the main go.mod so the mage targets stay self-contained.
type aggregatedCell struct {
	RPSMedian float64
}

// loadAggregate walks a matrix output directory, reads every per-cell
// JSON, groups by (scenario, server), and computes the median RPS.
func loadAggregate(dir string) (map[string]aggregatedCell, error) {
	byCell := map[string][]float64{}
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		if strings.HasSuffix(path, "manifest.json") {
			return nil
		}
		data, rErr := os.ReadFile(path)
		if rErr != nil {
			return nil
		}
		var cm cellMinimal
		if err := json.Unmarshal(data, &cm); err != nil || cm.Result == nil {
			return nil
		}
		id := cm.Scenario + "/" + cm.Server
		byCell[id] = append(byCell[id], cm.Result.RequestsPerSec)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	out := make(map[string]aggregatedCell, len(byCell))
	for id, vals := range byCell {
		out[id] = aggregatedCell{RPSMedian: medianSorted(vals)}
	}
	return out, nil
}

// medianSorted returns the median of a float64 slice without mutating it.
func medianSorted(vals []float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	sorted := make([]float64, n)
	copy(sorted, vals)
	// Small-N bubble sort is fine here; typical n is 10.
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// generateMatrixReport is the post-run hook invoked by runMatrix. It
// shells out to the perfmatrix report generator (which lives in the
// perfmatrix submodule) so we don't have to import the perfmatrix
// packages from the main module. The generator binary is tiny.
func generateMatrixReport(outDir string) error {
	// Build a small helper binary on the fly: `go run` a generator in
	// the perfmatrix module against outDir. We use `go run` so we don't
	// have to check in a compiled artefact; the source lives at
	// test/perfmatrix/cmd/reportgen.
	genDir := filepath.Join("test", "perfmatrix", "cmd", "reportgen")
	if _, err := os.Stat(genDir); os.IsNotExist(err) {
		// Fall back: just list the cell JSONs so the user knows the
		// matrix produced data.
		fmt.Printf("perfmatrix: report generator not found at %s; skipping aggregation\n", genDir)
		return nil
	}
	cmd := exec.Command("go", "run", "./cmd/reportgen", "-in", outDir)
	cmd.Dir = filepath.Join("test", "perfmatrix")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("reportgen: %w", err)
	}
	fmt.Printf("perfmatrix: report written to %s/report.md\n", outDir)
	return nil
}
