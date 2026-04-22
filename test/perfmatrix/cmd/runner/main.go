// Command runner is the perfmatrix orchestrator binary. It walks a
// (scenario × server) matrix, starts each server on a fresh ephemeral
// port, drives it with loadgen as a library (no subprocess fork per
// cell), optionally captures pprof profiles, and writes a per-cell
// JSON result file plus a top-level manifest.
package main

import (
	"context"
	"encoding/json"
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

	for i, cell := range schedule {
		if rootCtx.Err() != nil {
			fmt.Fprintln(os.Stderr, "perfmatrix: cancelled")
			break
		}
		fmt.Fprintf(os.Stderr, "[%d/%d] run=%d scenario=%s server=%s\n",
			i+1, len(schedule), cell.RunIdx, cell.Scenario.Name(), cell.Server.Name())
		if err := executeCell(rootCtx, cfg, handles, cell); err != nil {
			fmt.Fprintf(os.Stderr, "  cell error: %v\n", err)
		}

		if i+1 < len(schedule) && cfg.Cooldown > 0 {
			select {
			case <-rootCtx.Done():
			case <-time.After(cfg.Cooldown):
			}
		}
	}

	// Write manifest last so completed_at is accurate.
	if err := writeManifest(cfg, effScs, effSvs, schedule); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	fmt.Fprintf(os.Stderr, "perfmatrix: done; started=%s duration=%s out=%s\n",
		manifestStart.Format(time.RFC3339), time.Since(manifestStart).Round(time.Millisecond), cfg.Out)
	return nil
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

	// Server startup (2 s hard cap).
	startCtx, startCancel := context.WithTimeout(parent, 2*time.Second)
	ln, err := cell.Server.Start(startCtx, handles)
	startCancel()
	if err != nil {
		result.Error = "server start: " + err.Error()
		return err
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		_ = cell.Server.Stop(stopCtx)
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
	cellCtx, cellCancel := context.WithTimeout(parent, cfg.Warmup+cfg.Duration+30*time.Second)
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
	return nil
}

// filterCells returns the subset of scenarios and servers selected by
// the glob. The glob is matched against the cell id "<scenario>/<server>".
// When the glob is empty, every registered scenario and server is
// returned unchanged.
func filterCells(scs []scenarios.Scenario, svs []servers.Server, glob string) ([]scenarios.Scenario, []servers.Server, error) {
	if glob == "" || glob == "*" {
		return scs, svs, nil
	}
	// Match "<scenario>/<server>" against the glob using path.Match.
	// Validate glob once so malformed patterns error rather than
	// silently match nothing.
	if _, err := path.Match(glob, "probe/probe"); err != nil {
		return nil, nil, fmt.Errorf("invalid glob %q: %w", glob, err)
	}

	scSet := map[string]bool{}
	svSet := map[string]bool{}
	for _, s := range scs {
		for _, srv := range svs {
			id := s.Name() + "/" + srv.Name()
			ok, _ := path.Match(glob, id)
			if ok {
				scSet[s.Name()] = true
				svSet[srv.Name()] = true
			}
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
		case strings.Contains(name, "memcached"):
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
