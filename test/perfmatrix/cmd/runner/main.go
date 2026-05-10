// Command runner is the perfmatrix orchestrator binary. It walks a
// (scenario × server) matrix, starts each server on a fresh ephemeral
// port, drives it with loadgen as a library (no subprocess fork per
// cell), optionally captures pprof profiles, and writes a per-cell
// JSON result file plus a top-level manifest.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goceleris/loadgen"

	"github.com/goceleris/celeris/test/perfmatrix/interleave"
	"github.com/goceleris/celeris/test/perfmatrix/profiling"
	"github.com/goceleris/celeris/test/perfmatrix/scenarios"
	"github.com/goceleris/celeris/test/perfmatrix/servers"
	"github.com/goceleris/celeris/test/perfmatrix/services"
)

// Config is the parsed flag set. Exported for cmd/runner tests.
type Config struct {
	Runs      int
	Duration  time.Duration
	Warmup    time.Duration
	Cells     string
	Out       string
	Profile   bool
	Services  string
	MagePhase string
	Cooldown  time.Duration
	Seed      int64
	// FailFast aborts the run the first time a cell returns an error.
	// Use with `mage matrixBenchStrict` to catch concurrency / memory
	// safety bugs the moment they surface (otherwise a single -race
	// report or a SIGSEGV signal could sit buried for hours while the
	// rest of the matrix keeps running).
	FailFast bool
	// FDTrace emits the per-cell `cell-fd: before=N after=M diff=±K`
	// log line for every cell, not just cells with non-zero deltas.
	// Off by default — leak hunts that need absolute counts to spot
	// slow drift turn it on (PERFMATRIX_FD_TRACE=1 / -fd-trace).
	FDTrace bool
}

// DefaultConfig is the set of defaults applied to a fresh flag.FlagSet.
func DefaultConfig() Config {
	return Config{
		Runs:      10,
		Duration:  10 * time.Second,
		Warmup:    2 * time.Second,
		Cells:     "",
		Out:       "",
		Profile:   false,
		Services:  "local",
		MagePhase: "full",
		Cooldown:  2 * time.Second,
		Seed:      0, // 0 = use time.Now().UnixNano()
	}
}

// Bind registers every Config field onto fs. Separated from ParseArgs so
// unit tests can drive parsing deterministically.
func (c *Config) Bind(fs *flag.FlagSet) {
	fs.IntVar(&c.Runs, "runs", c.Runs,
		"number of interleaved passes through the matrix")
	fs.DurationVar(&c.Duration, "duration", c.Duration,
		"measurement window per cell")
	fs.DurationVar(&c.Warmup, "warmup", c.Warmup,
		"pre-measurement warmup per cell")
	fs.StringVar(&c.Cells, "cells", c.Cells,
		`glob filter over "<scenario>/<server>" (e.g. "celeris-*/get-json", "*/driver-*")`)
	fs.StringVar(&c.Out, "out", c.Out,
		"output directory; default results/<timestamp>-<git-ref>/")
	fs.BoolVar(&c.Profile, "profile", c.Profile,
		"enable pprof capture per cell (silently skipped for servers that do not implement ProfilePort)")
	fs.StringVar(&c.Services, "services", c.Services,
		`"local" (Docker on same host) | "none" (skip driver services)`)
	fs.StringVar(&c.MagePhase, "mage-phase", c.MagePhase,
		`internal; "full" | "quick" | "drivers" | "since"`)
	fs.DurationVar(&c.Cooldown, "cooldown", c.Cooldown,
		"idle gap between cells to let TCP TIME_WAIT drain")
	fs.Int64Var(&c.Seed, "seed", c.Seed,
		"rng seed for any randomised orchestrator behaviour; 0 = time.Now().UnixNano()")
	fs.BoolVar(&c.FailFast, "fail-fast", c.FailFast,
		"abort the run at the first cell error (pair with -race / GORACE=halt_on_error for strict mode)")
	fs.BoolVar(&c.FDTrace, "fd-trace", c.FDTrace,
		"log per-cell FD counts even when the delta is zero (off by default — non-zero deltas always log)")
}

// ParseArgs parses argv (without the program name). out is used for flag
// usage / error output; typically os.Stderr.
func ParseArgs(args []string, out io.Writer) (Config, error) {
	cfg := DefaultConfig()
	fs := flag.NewFlagSet("perfmatrix-runner", flag.ContinueOnError)
	fs.SetOutput(out)
	cfg.Bind(fs)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// cellResultFile is the on-disk JSON shape for a single cell run.
type cellResultFile struct {
	RunIdx       int              `json:"run_idx"`
	ScenarioName string           `json:"scenario"`
	ServerName   string           `json:"server"`
	ServerKind   string           `json:"server_kind"`
	Category     string           `json:"category"`
	TargetAddr   string           `json:"target_addr"`
	StartedAt    time.Time        `json:"started_at"`
	CompletedAt  time.Time        `json:"completed_at"`
	Error        string           `json:"error,omitempty"`
	Result       *loadgen.Result  `json:"result,omitempty"`
	Profile      *profileArtifact `json:"profile,omitempty"`
	// Resource accounting (linux only — zero elsewhere).
	// FDsBefore   = open FDs in the runner immediately before cell.Server.Start.
	// FDsAfterStop = open FDs immediately after cell.Server.Stop returned.
	// FDsLeaked   = FDsAfterStop - FDsBefore. Positive = the engine teardown
	//              path failed to close everything it opened during the cell.
	FDsBefore    int `json:"fds_before,omitempty"`
	FDsAfterStop int `json:"fds_after_stop,omitempty"`
	FDsLeaked    int `json:"fds_leaked,omitempty"`
}

// profileArtifact lists the pprof files captured for this cell, if any.
type profileArtifact struct {
	CPU       string `json:"cpu,omitempty"`
	Heap      string `json:"heap,omitempty"`
	Goroutine string `json:"goroutine,omitempty"`
	Skipped   string `json:"skipped,omitempty"` // "no ProfilePort" etc.
}

// manifest is the top-level summary of a run.
type manifest struct {
	Config      Config    `json:"config"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Host        hostInfo  `json:"host"`
	Seed        int64     `json:"seed"`
	GitSHA      string    `json:"git_sha,omitempty"`
	GoVersion   string    `json:"go_version"`
	LoadgenVer  string    `json:"loadgen_version,omitempty"`
	Scenarios   []string  `json:"scenarios"`
	Servers     []string  `json:"servers"`
	CellCount   int       `json:"cell_count"`
	Cells       []string  `json:"cells"`
}

// hostInfo captures the run environment.
type hostInfo struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname,omitempty"`
}

func main() {
	cfg, err := ParseArgs(os.Args[1:], os.Stderr)
	if err != nil {
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "perfmatrix: %v\n", err)
		os.Exit(1)
	}
}

// run is the top-level orchestrator entry point, separated from main so
// it's testable without exec'ing a subprocess.
func run(cfg Config) error {
	// Resolve seed so the actual seed is echoed to stderr for
	// reproducibility.
	if cfg.Seed == 0 {
		cfg.Seed = time.Now().UnixNano()
	}
	// math/rand/v2 has no global Seed(); we build a local rng keyed by
	// the seed for any future randomised decisions (scheduler tie-break
	// etc.). Today the scheduler is fully deterministic so the rng is
	// unused beyond the echo.
	_ = rand.New(rand.NewPCG(uint64(cfg.Seed), uint64(cfg.Seed)^0x9E3779B97F4A7C15))
	fmt.Fprintf(os.Stderr, "perfmatrix: seed=%d\n", cfg.Seed)

	// Resolve output directory. Default is
	// results/<timestamp>-<git-ref>/ relative to cwd.
	if cfg.Out == "" {
		gitRef := shortGitSHA()
		if gitRef == "" {
			gitRef = "local"
		}
		ts := time.Now().UTC().Format("20060102T150405Z")
		cfg.Out = filepath.Join("results", ts+"-"+gitRef)
	}
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", cfg.Out, err)
	}

	// Load registries. Server init() calls register their own cells.
	scs := scenarios.Registry()
	svs := servers.Registry()

	// Apply -cells glob filter.
	effScs, effSvs, err := filterCells(scs, svs, cfg.Cells)
	if err != nil {
		return fmt.Errorf("filter cells: %w", err)
	}

	// Service kinds: inspect which scenarios are in the effective set
	// and provision only what they need.
	svcKinds := requiredServiceKinds(effScs)
	if cfg.Services == "none" {
		svcKinds = nil
	} else if cfg.Services != "local" && cfg.Services != "" {
		return fmt.Errorf("unknown -services value %q (want \"local\" or \"none\")", cfg.Services)
	}

	// Install signal handler: one SIGINT/SIGTERM → graceful shutdown,
	// second → hard exit.
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	installSignalHandler(rootCancel)

	// Bring up services before any cells run.
	var handles *services.Handles
	if len(svcKinds) > 0 {
		fmt.Fprintf(os.Stderr, "perfmatrix: starting services %v\n", svcKinds)
		handles, err = services.Start(rootCtx, svcKinds...)
		if err != nil {
			return fmt.Errorf("start services: %w", err)
		}
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := handles.Stop(stopCtx); err != nil {
				fmt.Fprintf(os.Stderr, "perfmatrix: services.Stop: %v\n", err)
			}
		}()
		if err := handles.Seed(rootCtx); err != nil {
			return fmt.Errorf("seed services: %w", err)
		}
	}

	// Compute the execution schedule.
	schedule := interleave.Schedule(cfg.Runs, effScs, effSvs)
	if len(schedule) == 0 {
		fmt.Fprintln(os.Stderr, "perfmatrix: no cells matched; nothing to do")
		// Still write a manifest so automated consumers can observe the
		// empty run.
		if err := writeManifest(cfg, effScs, effSvs, schedule); err != nil {
			return err
		}
		return nil
	}

	manifestStart := time.Now().UTC()

	fmt.Fprintf(os.Stderr, "perfmatrix: %d cells across %d scenarios × %d servers × %d runs\n",
		len(schedule), len(effScs), len(effSvs), cfg.Runs)

	// Per-server FD-leak accumulator. Aggregates across all cells of
	// the same server name so a cell-by-cell leak (e.g. epoll teardown
	// missing one FD) shows up as a clear linear growth in the
	// per-server total at the end of the run.
	type leakStats struct {
		count   int
		total   int
		maxCell int // largest single-cell leak observed
	}
	leakBy := map[string]*leakStats{}
	preRunFDs := countProcessFDs()

	var firstCellErr error
	for i, cell := range schedule {
		if rootCtx.Err() != nil {
			fmt.Fprintln(os.Stderr, "perfmatrix: cancelled")
			break
		}
		fmt.Fprintf(os.Stderr, "[%d/%d] run=%d scenario=%s server=%s\n",
			i+1, len(schedule), cell.RunIdx, cell.Scenario.Name(), cell.Server.Name())
		// executeCell stamps FDsLeaked on the per-cell JSON via the
		// Stop defer; we don't have direct access to that here, so the
		// inline `cell-fd:` log line is the source of truth for the
		// per-cell delta. The post-run summary below scans the per-
		// cell JSON files to aggregate.
		if err := executeCell(rootCtx, cfg, handles, cell); err != nil {
			fmt.Fprintf(os.Stderr, "  cell error: %v\n", err)
			if cfg.FailFast {
				firstCellErr = fmt.Errorf("fail-fast: %s/%s: %w",
					cell.Scenario.Name(), cell.Server.Name(), err)
				break
			}
		}

		if i+1 < len(schedule) && cfg.Cooldown > 0 {
			select {
			case <-rootCtx.Done():
			case <-time.After(cfg.Cooldown):
			}
		}
	}

	// FD-leak post-run summary. Reads every per-cell JSON we wrote and
	// groups by server name. Top offenders are printed first; a single
	// server name showing leak/cell ≈ constant is the smoking gun for
	// a teardown path that misses one FD per cell.
	postRunFDs := countProcessFDs()
	if err := filepath.Walk(cfg.Out, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		blob, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		var c cellResultFile
		if jerr := json.Unmarshal(blob, &c); jerr != nil || c.FDsLeaked <= 0 {
			return nil
		}
		s := leakBy[c.ServerName]
		if s == nil {
			s = &leakStats{}
			leakBy[c.ServerName] = s
		}
		s.count++
		s.total += c.FDsLeaked
		if c.FDsLeaked > s.maxCell {
			s.maxCell = c.FDsLeaked
		}
		return nil
	}); err == nil && len(leakBy) > 0 {
		type row struct {
			server string
			st     *leakStats
		}
		rows := make([]row, 0, len(leakBy))
		for sv, st := range leakBy {
			rows = append(rows, row{sv, st})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].st.total > rows[j].st.total })
		fmt.Fprintf(os.Stderr, "perfmatrix: FD-leak summary — pre-run=%d post-run=%d delta=%d\n",
			preRunFDs, postRunFDs, postRunFDs-preRunFDs)
		fmt.Fprintln(os.Stderr, "perfmatrix: top leakers (server / leaking-cells / total / max-per-cell):")
		for i, r := range rows {
			if i >= 15 {
				break
			}
			avg := float64(r.st.total) / float64(r.st.count)
			fmt.Fprintf(os.Stderr, "  %-50s cells=%-5d total=%-6d max=%-4d avg=%.2f\n",
				r.server, r.st.count, r.st.total, r.st.maxCell, avg)
		}
	}

	// Write manifest last so completed_at is accurate.
	if err := writeManifest(cfg, effScs, effSvs, schedule); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	fmt.Fprintf(os.Stderr, "perfmatrix: done; started=%s duration=%s out=%s\n",
		manifestStart.Format(time.RFC3339), time.Since(manifestStart).Round(time.Millisecond), cfg.Out)
	return firstCellErr
}

// executeCell runs one (scenario, server, run) tuple end-to-end. Errors
// are surfaced to the caller so they can be logged; a per-cell JSON
// artefact is always written (even on failure) so release-gate review
// can spot missing cells.
func executeCell(parent context.Context, cfg Config, handles *services.Handles, cell interleave.Cell) error {
	outDir := filepath.Join(cfg.Out, fmt.Sprintf("run%d", cell.RunIdx), cell.Scenario.Name())
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	outFile := filepath.Join(outDir, cell.Server.Name()+".json")

	result := cellResultFile{
		RunIdx:       cell.RunIdx,
		ScenarioName: cell.Scenario.Name(),
		ServerName:   cell.Server.Name(),
		ServerKind:   cell.Server.Kind(),
		Category:     cell.Scenario.Category(),
		StartedAt:    time.Now().UTC(),
	}
	defer func() {
		result.CompletedAt = time.Now().UTC()
		if err := writeJSON(outFile, &result); err != nil {
			fmt.Fprintf(os.Stderr, "  write %s: %v\n", outFile, err)
		}
	}()

	// FD accounting: count process FDs immediately before Server.Start
	// so the after-Stop count gives a clean delta. The leak hunt that
	// triggered this instrumentation traced 939 orphan socket FDs on
	// msr1 across ~2000 cells — a per-cell delta isolates which engine
	// teardown path is responsible.
	result.FDsBefore = countProcessFDs()

	// Server startup (2 s hard cap).
	startCtx, startCancel := context.WithTimeout(parent, 2*time.Second)
	ln, err := cell.Server.Start(startCtx, handles)
	startCancel()
	if err != nil {
		result.Error = "server start: " + err.Error()
		return err
	}
	defer func() {
		// 15 s: native celeris engines (epoll / iouring / adaptive) need
		// their ctx cancelled + worker-goroutine drain before they
		// release OS threads. 1 s was not enough — the matrix hit Go's
		// 10000-thread ceiling around cell 1021 in the last run.
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = cell.Server.Stop(stopCtx)
		// FD-leak accounting: count again post-Stop and stamp the delta
		// onto the result. Logged inline so the leak shows up in the
		// matrix log even when the per-cell JSON is missed (fail-fast
		// abort, Ctrl-C, …). Negative deltas are normal — long-lived
		// listen FDs from prior cells can be reaped during this cell's
		// Stop. We surface only positive deltas at log time.
		result.FDsAfterStop = countProcessFDs()
		result.FDsLeaked = result.FDsAfterStop - result.FDsBefore
		// Default: only log on a non-zero delta — keeps gate.log
		// readable. PERFMATRIX_FD_TRACE=1 forces per-cell before/after
		// for active leak hunts (slow upward drift over thousands of
		// cells is invisible without absolute counts). Either way the
		// values land on the per-cell JSON via FDsBefore/FDsAfterStop
		// for post-run analysis.
		if result.FDsLeaked != 0 || cfg.FDTrace {
			fmt.Fprintf(os.Stderr, "  cell-fd: scenario=%s server=%s before=%d after=%d diff=%+d\n",
				cell.Scenario.Name(), cell.Server.Name(),
				result.FDsBefore, result.FDsAfterStop, result.FDsLeaked)
		}
	}()
	target := ln.Addr()
	targetAddr := target.String()
	result.TargetAddr = targetAddr

	// Ready-check the bound address before handing to loadgen.
	if err := waitForTCP(parent, targetAddr, 2*time.Second); err != nil {
		result.Error = "server ready-check: " + err.Error()
		return err
	}

	// Build loadgen config from the scenario.
	lgURL := "http://" + targetAddr
	lgCfg := cell.Scenario.Workload(lgURL)
	if lgCfg.URL == "" {
		// Some scaffolded scenarios return a zero Config; inject a
		// sensible default so the cell still runs a measurement.
		lgCfg.URL = lgURL + "/"
	}
	lgCfg.Duration = cfg.Duration
	lgCfg.Warmup = cfg.Warmup
	// Scenario Workload() intentionally leaves Workers unset when the
	// scenario author wants the orchestrator to pick a sensible default.
	// Match loadgen's DefaultConfig so library callers get the same
	// behaviour as the CLI.
	if lgCfg.Workers == 0 {
		lgCfg.Workers = 64
	}

	// Profile capture, if enabled and supported.
	var profileArt *profileArtifact
	// Per-cell timeout. The original Warmup+Duration+30s was too tight
	// under the strict matrix (-race + checkptr) on slower hosts —
	// observed on msr1 (aarch64) where the 1MB POST scenario's
	// loadgen warmup naturally stretched well past 1s under -race
	// overhead. When warmup eats most of the budget, post-warmup
	// returns with 0 requests and the cell mis-fires fail-fast on a
	// non-bug. Scaled buffer (5x Warmup + 2x Duration + 60s) keeps
	// fast cells fast while letting slow ones complete, and the
	// 0-req guard still catches genuine failures (server refused
	// every connection, hung mid-handshake).
	cellTimeout := cellTimeoutFor(cfg.Warmup, cfg.Duration)
	cellCtx, cellCancel := context.WithTimeout(parent, cellTimeout)
	defer cellCancel()

	var cpuWG sync.WaitGroup
	if cfg.Profile {
		if p, ok := cell.Server.(profiling.Profileable); ok && p.ProfilePort() > 0 {
			pcfg := profiling.Config{
				TargetURL:     fmt.Sprintf("http://127.0.0.1:%d", p.ProfilePort()),
				OutDir:        outDir,
				CPUDur:        cfg.Duration,
				CPUFile:       cell.Server.Name() + ".cpu.pprof",
				HeapFile:      cell.Server.Name() + ".heap.pprof",
				GoroutineFile: cell.Server.Name() + ".goroutine.pprof",
			}
			profileArt = &profileArtifact{}
			// CPU capture runs in parallel with the measurement window.
			// loadgen also runs the warmup in-band, so the CPU profile
			// starts at warmup-end.
			cpuWG.Go(func() {
				// Give loadgen.warmup a head start before sampling CPU
				// so the profile reflects steady-state.
				select {
				case <-cellCtx.Done():
					return
				case <-time.After(cfg.Warmup):
				}
				path, cpuErr := profiling.CaptureCPU(cellCtx, pcfg)
				if cpuErr == nil {
					profileArt.CPU = path
				} else {
					fmt.Fprintf(os.Stderr, "  CaptureCPU: %v\n", cpuErr)
				}
			})
			// Heap + goroutine captured below after Run returns.
			defer func(p profiling.Config) {
				if hp, err := profiling.CaptureHeap(context.Background(), p); err == nil {
					profileArt.Heap = hp
				}
				if gp, err := profiling.CaptureGoroutine(context.Background(), p); err == nil {
					profileArt.Goroutine = gp
				}
			}(pcfg)
		} else {
			fmt.Fprintf(os.Stderr, "  cell %s/%s not profileable; skipping pprof capture\n",
				cell.Scenario.Name(), cell.Server.Name())
			profileArt = &profileArtifact{Skipped: "server does not implement Profileable"}
		}
	}

	// Run loadgen.
	bm, err := loadgen.New(lgCfg)
	if err != nil {
		result.Error = "loadgen.New: " + err.Error()
		cpuWG.Wait()
		return err
	}
	res, err := bm.Run(cellCtx)
	cpuWG.Wait()
	if err != nil {
		result.Error = "loadgen.Run: " + err.Error()
		return err
	}
	result.Result = res
	result.Profile = profileArt
	// Strict 0-request guard. A cell that measured zero requests almost
	// always means the server silently refused to serve the client's
	// wire protocol (scenario × server mismatch) or wedged mid-warmup
	// — both are test-validity bugs we want to surface, not data
	// points to average into a report. Raising this as a cell error
	// lets -fail-fast stop the run so it can be investigated before
	// polluting hours of downstream measurements.
	if res != nil && res.Requests == 0 {
		// Capture process-level resource state at the moment of failure.
		// 0-req cells are often tail-of-matrix accumulation symptoms (FD
		// or goroutine leak, TCP TIME_WAIT exhaustion); dumping these
		// counters into the cell error gives the next investigator a
		// concrete starting point instead of "look at the whole matrix
		// log and guess". Cheap to compute, only fires on failure.
		diag := zeroReqDiag()
		result.Error = fmt.Sprintf("zero-request cell: errors=%d duration=%s diag=[%s] (server likely refused the client's protocol, accumulated state exhausted resources, or hung mid-warmup)",
			res.Errors, res.Duration, diag)
		return errors.New(result.Error)
	}
	return nil
}

// cellTimeoutFor computes the per-cell ctx deadline. Pinning a single
// constant collapses two failure modes (cells over-running their fast
// duration; runtime overhead under -race) into one tunable formula.
//
// The 5x Warmup factor absorbs the cost of -race + -checkptr=2 setup
// inside loadgen / server processes. The 2x Duration factor leaves
// room for graceful drain. The 60s constant covers signal-handling
// shutdown, FD leak diag, and per-cell file writes. Anything inside
// this envelope is a real failure (server hung, deadlock); anything
// outside is genuine timeout.
func cellTimeoutFor(warmup, duration time.Duration) time.Duration {
	return 5*warmup + 2*duration + 60*time.Second
}

// countProcessFDs returns the count of open file descriptors in the
// runner process by reading /proc/self/fd. Returns 0 on platforms
// without procfs (macOS dev runs) — leak detection is meaningful only
// on the linux benchmark hosts where /proc is canonical.
func countProcessFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}

// zeroReqDiag captures a one-line snapshot of process resources useful
// for diagnosing 0-request failures that only surface deep into a
// long-running matrix. Goroutine count exposes worker leaks; open-FD
// count exposes socket/listener leaks; ephemeral-port count exposes
// TCP TIME_WAIT pool exhaustion.
func zeroReqDiag() string {
	var b strings.Builder
	fmt.Fprintf(&b, "goroutines=%d", runtime.NumGoroutine())

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintf(&b, " heap_inuse=%d sys=%d", ms.HeapInuse, ms.Sys)

	if entries, err := os.ReadDir("/proc/self/fd"); err == nil {
		fmt.Fprintf(&b, " open_fds=%d", len(entries))
	}
	// Loopback ephemeral port pressure: count TIME_WAIT lines in
	// /proc/net/tcp{,6}. Cheap parse — we only need the count.
	tw := 0
	for _, p := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if blob, err := os.ReadFile(p); err == nil {
			for _, line := range strings.Split(string(blob), "\n") {
				fields := strings.Fields(line)
				// fields[3] is the state column; 06 = TCP_TIME_WAIT
				if len(fields) >= 4 && fields[3] == "06" {
					tw++
				}
			}
		}
	}
	fmt.Fprintf(&b, " time_wait=%d", tw)
	// Per-process limit + current usage from /proc/self/limits, picked
	// out for "Max open files" only.
	if blob, err := os.ReadFile("/proc/self/limits"); err == nil {
		for _, line := range strings.Split(string(blob), "\n") {
			if strings.HasPrefix(line, "Max open files") {
				fmt.Fprintf(&b, " %s", strings.Join(strings.Fields(line), "_"))
				break
			}
		}
	}
	return b.String()
}

// filterCells returns the subset of scenarios and servers selected by
// the glob. The glob is matched against the cell id "<scenario>/<server>".
// When the glob is empty, every registered scenario and server is
// returned unchanged.
//
// The filter accepts a comma-separated list of patterns. Each pattern
// is either a positive glob (include) or a negative glob prefixed with
// "!" (exclude). A cell is kept if it matches at least one positive
// pattern AND no negative pattern. When every pattern is a negation,
// the positive set defaults to "*". Example:
//
//	"*"                                  — every cell
//	"driver-*/*"                         — only driver scenarios
//	"*,!*/celeris-std-h2c*"              — every cell, skip std+h2c
//	"!*/celeris-std-h2c*,!*/stdhttp-h2c" — everything except x/net h2c
//
// The final two forms are what matrixBenchStrict uses to skip cells
// that trigger a known upstream race in x/net/http2/hpack (which is
// not a celeris bug and would otherwise trip fail-fast every run).
func filterCells(scs []scenarios.Scenario, svs []servers.Server, glob string) ([]scenarios.Scenario, []servers.Server, error) {
	if glob == "" || glob == "*" {
		return scs, svs, nil
	}

	var include, exclude []string
	for _, part := range strings.Split(glob, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "!") {
			exclude = append(exclude, p[1:])
		} else {
			include = append(include, p)
		}
	}
	if len(include) == 0 {
		include = []string{"*"} // pure-exclusion filter
	}

	// Validate globs up-front so malformed patterns error rather
	// than silently match nothing.
	for _, g := range append(include, exclude...) {
		if _, err := path.Match(g, "probe/probe"); err != nil {
			return nil, nil, fmt.Errorf("invalid glob %q: %w", g, err)
		}
	}

	matchAny := func(patterns []string, id string) bool {
		for _, g := range patterns {
			// "*" as a top-level pattern means "every cell"; path.Match
			// treats "*" as non-separator-only so it would reject
			// "scenario/server" on the slash. Short-circuit here so
			// the universal include works as users expect.
			if g == "*" {
				return true
			}
			if ok, _ := path.Match(g, id); ok {
				return true
			}
		}
		return false
	}

	scSet := map[string]bool{}
	svSet := map[string]bool{}
	for _, s := range scs {
		for _, srv := range svs {
			id := s.Name() + "/" + srv.Name()
			if !matchAny(include, id) {
				continue
			}
			if matchAny(exclude, id) {
				continue
			}
			scSet[s.Name()] = true
			svSet[srv.Name()] = true
		}
	}
	var outS []scenarios.Scenario
	for _, s := range scs {
		if scSet[s.Name()] {
			outS = append(outS, s)
		}
	}
	var outV []servers.Server
	for _, srv := range svs {
		if svSet[srv.Name()] {
			outV = append(outV, srv)
		}
	}
	return outS, outV, nil
}

// requiredServiceKinds walks the effective scenario set and returns the
// set of service Kind constants whose backing datastores are needed.
// Uses the scenario Category() to decide: only "driver" scenarios
// require services.
func requiredServiceKinds(scs []scenarios.Scenario) []string {
	need := map[string]bool{}
	for _, s := range scs {
		if s.Category() != scenarios.CategoryDriver {
			continue
		}
		// Driver scenarios encode their target kind in the name, e.g.
		// "driver-pg-read" → postgres.
		name := s.Name()
		switch {
		case strings.Contains(name, "pg") || strings.Contains(name, "postgres"):
			need[services.KindPostgres] = true
		case strings.Contains(name, "redis"):
			need[services.KindRedis] = true
		case strings.Contains(name, "memcached") || strings.Contains(name, "-mc-"):
			// Match both the long form ("driver-memcached-*") and the short
			// form ("driver-mc-get") so scenario authors can use either.
			need[services.KindMemcached] = true
		case strings.Contains(name, "session"):
			// Session scenarios use Redis by convention.
			need[services.KindRedis] = true
		}
	}
	out := make([]string, 0, len(need))
	for k := range need {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// waitForTCP dials addr with a 50 ms backoff until either the dial
// succeeds or the timeout elapses. Matches the services package helper
// but lives here so the orchestrator has no import cycle.
func waitForTCP(ctx context.Context, addr string, timeout time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	for {
		conn, err := d.DialContext(wctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("tcp probe %s: %w", addr, wctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// installSignalHandler wires SIGINT/SIGTERM to cancel the root context.
// A second signal bypasses graceful shutdown and exits immediately.
func installSignalHandler(cancel context.CancelFunc) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		fmt.Fprintln(os.Stderr, "perfmatrix: interrupted, shutting down")
		cancel()
		<-ch
		fmt.Fprintln(os.Stderr, "perfmatrix: second signal, forcing exit")
		os.Exit(130)
	}()
}

// writeJSON marshals v to path with 2-space indentation. Creates the
// parent directory if missing.
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o644)
}

// writeManifest dumps a top-level manifest.json summarising the run.
func writeManifest(cfg Config, scs []scenarios.Scenario, svs []servers.Server, sched []interleave.Cell) error {
	host := hostInfo{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if hn, err := os.Hostname(); err == nil {
		host.Hostname = hn
	}

	m := manifest{
		Config:      cfg,
		StartedAt:   time.Now().UTC(), // approx — run() stamps real values
		CompletedAt: time.Now().UTC(),
		Host:        host,
		Seed:        cfg.Seed,
		GitSHA:      shortGitSHA(),
		GoVersion:   runtime.Version(),
		LoadgenVer:  loadgenVersion(),
		CellCount:   len(sched),
	}
	for _, s := range scs {
		m.Scenarios = append(m.Scenarios, s.Name())
	}
	for _, s := range svs {
		m.Servers = append(m.Servers, s.Name())
	}
	for _, c := range sched {
		m.Cells = append(m.Cells, fmt.Sprintf("run%d/%s/%s", c.RunIdx, c.Scenario.Name(), c.Server.Name()))
	}
	return writeJSON(filepath.Join(cfg.Out, "manifest.json"), &m)
}

// shortGitSHA returns the abbreviated HEAD SHA, or "" if git is not
// available or the orchestrator is not inside a repo.
func shortGitSHA() string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// loadgenVersion returns the loadgen module version parsed from the
// perfmatrix go.mod. We fall back to "" if the module info is not
// embedded — better to emit an empty string than to block the manifest.
func loadgenVersion() string {
	// runtime/debug.ReadBuildInfo would work inside the built binary but
	// we keep the dependency list minimal and defer this to wave-4 which
	// has to ship build info for the release reports anyway.
	return ""
}
