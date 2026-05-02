//go:build mage

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Cluster bench orchestration. Replaces the multipass VM machinery with
// SSH/Ansible-driven runs against the 3-node fabric (msa2-server,
// msa2-client, msr1) wired through the QSW-M3216R-8S8T switch.
//
// Pristine semantics: any apt packages we install on cluster nodes are
// recorded in a manifest and uninstalled after the bench. All transient
// state (binaries, logs, raw results) lives under /tmp on each node so
// the next reboot wipes it. See ansible/README.md.

const (
	clusterAnsibleDir = "ansible"
	clusterBenchPlaybook   = "cluster-bench.yml"
	clusterCleanupPlaybook = "cluster-cleanup.yml"

	// runnerModuleDir — the perfmatrix orchestrator lives in its own
	// Go module (test/perfmatrix/go.mod). We cd in there before building.
	runnerModuleDir = "test/perfmatrix"
	runnerPkgRel    = "./cmd/runner"
	// loadgen path — staged on msa2-client, planned for distributed
	// scenarios. For now we ship the same runner binary; loadgen
	// integration lives in the goceleris/loadgen repo.
)

// ClusterStatus prints quick health for each cluster node: reachability,
// SSH, dep manifest, latest results dir size. Cheap pre-flight check.
func ClusterStatus() error {
	if err := requireAnsible(); err != nil {
		return err
	}
	args := []string{
		"-i", "inventory.yml", "cluster",
		"-m", "shell",
		"-a", "uptime -p; echo --; ls /tmp/celeris-bench-manifest.json 2>/dev/null && cat /tmp/celeris-bench-manifest.json 2>/dev/null || echo 'no manifest'; echo --; du -sh /tmp/celeris-results 2>/dev/null || echo 'no results'",
	}
	cmd := exec.Command("ansible", args...)
	cmd.Dir = clusterAnsibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ClusterDeploy cross-compiles the runner binary for both archs and
// pushes it (plus loadgen) to /tmp/celeris-bench/ on the appropriate
// hosts. No bench is executed. Idempotent.
func ClusterDeploy() error {
	if err := requireAnsible(); err != nil {
		return err
	}
	bins, err := stageBinaries()
	if err != nil {
		return err
	}
	defer cleanupStaging(bins)

	args := []string{
		"-i", "inventory.yml",
		"--tags", "stage",
		clusterBenchPlaybook,
		"--extra-vars", "bench_targets_filter=both",
		"--extra-vars", "runner_binary_amd64=" + bins.runnerAmd64,
		"--extra-vars", "runner_binary_arm64=" + bins.runnerArm64,
		"--extra-vars", "loadgen_binary_amd64=" + bins.loadgenAmd64,
		"--extra-vars", "results_local_dir=" + bins.resultsLocal,
	}
	cmd := exec.Command("ansible-playbook", args...)
	cmd.Dir = clusterAnsibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ClusterBench is the unified bench target. By default runs the
// perfmatrix runner against both bench targets (msa2-server and msr1)
// in parallel with msa2-client deployed (loadgen reserved for
// distributed scenarios — single-host bench for now).
//
// Knobs (env):
//
//	CLUSTER_TARGETS      both | msa2-server | msr1   (default: both)
//	CLUSTER_RUNS         int                          (default: 3)
//	CLUSTER_DURATION     duration                     (default: 5s)
//	CLUSTER_WARMUP       duration                     (default: 1s)
//	CLUSTER_CELLS        runner -cells glob           (default: */get-simple-1024c)
//	CLUSTER_FULL_MATRIX  0|1                          (1 → -cells "*", overrides CELLS)
func ClusterBench() error {
	if err := requireAnsible(); err != nil {
		return err
	}

	targets := envOrDefault("CLUSTER_TARGETS", "both")
	runs := envOrDefault("CLUSTER_RUNS", "3")
	duration := envOrDefault("CLUSTER_DURATION", "5s")
	warmup := envOrDefault("CLUSTER_WARMUP", "1s")
	cells := envOrDefault("CLUSTER_CELLS", "get-simple-1024c/*")
	if os.Getenv("CLUSTER_FULL_MATRIX") == "1" {
		cells = "*"
	}

	bins, err := stageBinaries()
	if err != nil {
		return err
	}
	defer cleanupStaging(bins)

	fmt.Printf("\n=== Cluster bench ===\n")
	fmt.Printf("  targets:  %s\n", targets)
	fmt.Printf("  runs:     %s\n", runs)
	fmt.Printf("  duration: %s\n", duration)
	fmt.Printf("  warmup:   %s\n", warmup)
	fmt.Printf("  cells:    %s\n", cells)
	fmt.Printf("  results:  %s\n\n", bins.resultsLocal)

	args := []string{
		"-i", "inventory.yml", clusterBenchPlaybook,
		"--extra-vars", "bench_targets_filter=" + targets,
		"--extra-vars", "runner_binary_amd64=" + bins.runnerAmd64,
		"--extra-vars", "runner_binary_arm64=" + bins.runnerArm64,
		"--extra-vars", "loadgen_binary_amd64=" + bins.loadgenAmd64,
		"--extra-vars", "bench_cells=" + cells,
		"--extra-vars", "bench_runs=" + runs,
		"--extra-vars", "bench_duration=" + duration,
		"--extra-vars", "bench_warmup=" + warmup,
		"--extra-vars", "results_local_dir=" + bins.resultsLocal,
	}
	cmd := exec.Command("ansible-playbook", args...)
	cmd.Dir = clusterAnsibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cluster bench failed: %w", err)
	}

	fmt.Printf("\n=== Cluster bench complete. Results in %s ===\n", bins.resultsLocal)
	return nil
}

// ClusterCleanup forces the cleanup phase across all nodes. Use after a
// failed/interrupted bench to ensure no apt packages or staging dirs
// are left behind.
//
// Set CLUSTER_PURGE_RESULTS=1 to also drop /tmp/celeris-results/ on the
// nodes (rare — they're cleared at reboot anyway).
func ClusterCleanup() error {
	if err := requireAnsible(); err != nil {
		return err
	}
	purge := os.Getenv("CLUSTER_PURGE_RESULTS")
	args := []string{"-i", "inventory.yml", clusterCleanupPlaybook}
	if purge == "1" {
		args = append(args, "--extra-vars", "purge_results=true")
	}
	cmd := exec.Command("ansible-playbook", args...)
	cmd.Dir = clusterAnsibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// clusterBins captures the cross-compiled binaries plus the local
// results directory the playbook fetches into.
type clusterBins struct {
	stagingDir   string // temp dir on dev machine holding cross-compiled binaries
	runnerAmd64  string
	runnerArm64  string
	loadgenAmd64 string
	resultsLocal string
}

// stageBinaries cross-compiles the perfmatrix runner for both archs
// and prepares the results landing dir on the dev machine. The temp
// dir is removed by cleanupStaging after the playbook completes.
func stageBinaries() (*clusterBins, error) {
	stagingDir, err := os.MkdirTemp("", "celeris-cluster-stage-")
	if err != nil {
		return nil, err
	}
	bins := &clusterBins{
		stagingDir:   stagingDir,
		runnerAmd64:  filepath.Join(stagingDir, "runner-amd64"),
		runnerArm64:  filepath.Join(stagingDir, "runner-arm64"),
		loadgenAmd64: filepath.Join(stagingDir, "loadgen-amd64"),
	}

	ts := time.Now().UTC().Format("20060102-150405")
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	bins.resultsLocal = filepath.Join(cwd, "results", ts+"-cluster")
	if err := os.MkdirAll(bins.resultsLocal, 0o755); err != nil {
		return nil, err
	}

	fmt.Println("Cross-compiling runner for linux/amd64...")
	if err := crossCompileInDir(runnerModuleDir, runnerPkgRel, bins.runnerAmd64, "amd64"); err != nil {
		return nil, fmt.Errorf("cross-compile amd64: %w", err)
	}
	fmt.Println("Cross-compiling runner for linux/arm64...")
	if err := crossCompileInDir(runnerModuleDir, runnerPkgRel, bins.runnerArm64, "arm64"); err != nil {
		return nil, fmt.Errorf("cross-compile arm64: %w", err)
	}

	fmt.Println("Cross-compiling loadgen for linux/amd64...")
	if err := buildLoadgenAmd64(bins.loadgenAmd64); err != nil {
		return nil, fmt.Errorf("cross-compile loadgen: %w", err)
	}

	return bins, nil
}

// buildLoadgenAmd64 cross-compiles the goceleris/loadgen CLI for
// linux/amd64. Tries (in order):
//  1. Sibling clone — walks up from cwd looking for any ancestor with
//     a "loadgen/cmd/loadgen" subtree. Typical dev layout has celeris
//     and loadgen as siblings under a single goceleris/ root.
//  2. Temp go.mod that requires github.com/goceleris/loadgen, then
//     `go build -o <out>` the cmd path. This sidesteps the "go install
//     cannot cross-compile when GOBIN is set" restriction.
//
// Path 1 is preferred because it builds whatever the developer has
// locally; path 2 is the fallback for clean machines / CI.
func buildLoadgenAmd64(outputPath string) error {
	absOut, err := filepath.Abs(outputPath)
	if err != nil {
		return err
	}

	if siblingPath, ok := findLoadgenSibling(); ok {
		fmt.Printf("  building from sibling clone at %s\n", siblingPath)
		cmd := exec.Command("go", "build", "-o", absOut, "./cmd/loadgen")
		cmd.Dir = siblingPath
		cmd.Env = append(os.Environ(),
			"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0",
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	fmt.Println("  no sibling loadgen clone — fetching via temp module")
	tmpDir, err := os.MkdirTemp("", "celeris-loadgen-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	// Bootstrap a one-shot module that depends on loadgen.
	gomod := "module loadgen-builder\n\ngo 1.26\n\nrequire github.com/goceleris/loadgen latest\n"
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(gomod), 0o644); err != nil {
		return err
	}
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = tmpDir
	tidy.Stdout = os.Stdout
	tidy.Stderr = os.Stderr
	if err := tidy.Run(); err != nil {
		return fmt.Errorf("loadgen go mod tidy: %w", err)
	}
	build := exec.Command("go", "build", "-o", absOut, "github.com/goceleris/loadgen/cmd/loadgen")
	build.Dir = tmpDir
	build.Env = append(os.Environ(),
		"GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0",
	)
	build.Stdout = os.Stdout
	build.Stderr = os.Stderr
	return build.Run()
}

// findLoadgenSibling walks up from cwd looking for a directory at any
// ancestor level that contains a "loadgen/cmd/loadgen" subtree.
// Returns the absolute path to the loadgen repo root if found.
func findLoadgenSibling() (string, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	dir := cwd
	for {
		candidate := filepath.Join(dir, "loadgen", "cmd", "loadgen", "main.go")
		if _, err := os.Stat(candidate); err == nil {
			return filepath.Join(dir, "loadgen"), true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func cleanupStaging(b *clusterBins) {
	if b == nil || b.stagingDir == "" {
		return
	}
	_ = os.RemoveAll(b.stagingDir)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()

	buf := make([]byte, 1<<20)
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr != nil {
			break
		}
	}
	return nil
}

// crossCompileInDir runs `go build` from a sub-module directory so its
// own go.mod resolves dependencies. Output path is absolute (or relative
// to the dev-machine cwd; this works across nested modules).
func crossCompileInDir(moduleDir, pkgRel, outputPath, arch string) error {
	absOut, err := filepath.Abs(outputPath)
	if err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-o", absOut, pkgRel)
	cmd.Dir = moduleDir
	cmd.Env = append(os.Environ(),
		"GOOS=linux",
		"GOARCH="+arch,
		"CGO_ENABLED=0",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func requireAnsible() error {
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		return fmt.Errorf("ansible not installed — run `brew install ansible` (see ansible/README.md)")
	}
	return nil
}
