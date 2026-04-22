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
}

// MatrixBench runs the full release-gate performance matrix: all celeris
// configs × all competitors × all scenarios × 10 interleaved runs at 10s
// each. Expected runtime ~2.5 days on msr1. See test/perfmatrix/README.md.
func MatrixBench() error {
	return runMatrix(matrixFlags{
		runs:     10,
		duration: 10 * time.Second,
		warmup:   2 * time.Second,
		cells:    "*",
		profile:  false,
		services: "local",
	})
}

// MatrixBenchDeep is the maximum-rigor variant: 10 runs × 15s measurement +
// 3s warmup. Captures reliable p99.99 tails. ~3.5 days on msr1. Use for
// major-release gating.
func MatrixBenchDeep() error {
	return runMatrix(matrixFlags{
		runs:     10,
		duration: 15 * time.Second,
		warmup:   3 * time.Second,
		cells:    "*",
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

	args := []string{
		"run", "./cmd/runner",
		"-runs", strconv.Itoa(fl.runs),
		"-duration", fl.duration.String(),
		"-warmup", fl.warmup.String(),
		"-services", fl.services,
	}
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
	args = append(args, fl.extraArgs...)

	fmt.Printf("perfmatrix: go %s\n", strings.Join(args, " "))
	cmd := exec.Command("go", args...)
	cmd.Dir = filepath.Join("test", "perfmatrix")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
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
