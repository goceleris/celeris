//go:build mage

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	clusterAnsibleDir            = "ansible"
	clusterBenchPlaybook         = "cluster-bench.yml"
	clusterCleanupPlaybook       = "cluster-cleanup.yml"
	clusterDistributedPlaybook   = "cluster-distributed-bench.yml"
	clusterGoGatePlaybook        = "cluster-go-gate.yml"

	// perfmatrix lives in its own Go module (test/perfmatrix/go.mod).
	// We cd in there before building runner/server.
	perfmatrixModuleDir = "test/perfmatrix"
	runnerPkgRel        = "./cmd/runner"
	serverPkgRel        = "./cmd/server"
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
// hosts. Runs ClusterCleanup first so a prior run's stale staging
// (e.g. a SIGKILL'd ClusterGoGate that bypassed its always-block) is
// dropped before we re-stage; otherwise leftover Go toolchain at
// /tmp/celeris-bench/go/ would still be present for the next run.
// Idempotent.
func ClusterDeploy() error {
	if err := requireAnsible(); err != nil {
		return err
	}
	if err := ClusterCleanup(); err != nil {
		// Cleanup must not block deploy on a freshly provisioned host
		// (no manifest yet → cleanup is a no-op anyway). Log + continue.
		fmt.Fprintf(os.Stderr, "ClusterDeploy: pre-clean warning: %v\n", err)
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

// ClusterDistributedBench runs a network-bound bench: celeris server
// on one bench target (msa2-server or msr1), loadgen on msa2-client,
// driving load over the 20G LACP fabric. Pairwise — to bench both
// targets, run the target twice with different CLUSTER_DIST_TARGET.
//
// Knobs (env):
//
//	CLUSTER_DIST_TARGET    msa2-server | msr1     (default: msa2-server)
//	CLUSTER_DIST_SERVER    perfmatrix server name (default: celeris-epoll-h1-async)
//	CLUSTER_DIST_PORT      bind port              (default: 8080)
//	CLUSTER_DIST_PATH      loadgen URL path       (default: /)
//	CLUSTER_DIST_DURATION  duration               (default: 10s)
//	CLUSTER_DIST_WARMUP    warmup                 (default: 2s)
//	CLUSTER_DIST_CONNS     loadgen connections    (default: 256)
//	CLUSTER_DIST_WORKERS   loadgen workers        (default: 0 = library default)
//	CLUSTER_DIST_H2        loadgen -h2 flag           (default: false)
//	CLUSTER_DIST_H2C_UPGRADE loadgen -h2c-upgrade flag (default: false)
func ClusterDistributedBench() error {
	if err := requireAnsible(); err != nil {
		return err
	}

	target := envOrDefault("CLUSTER_DIST_TARGET", "msa2-server")
	serverName := envOrDefault("CLUSTER_DIST_SERVER", "celeris-epoll-h1-async")
	port := envOrDefault("CLUSTER_DIST_PORT", "8080")
	urlPath := envOrDefault("CLUSTER_DIST_PATH", "/")
	duration := envOrDefault("CLUSTER_DIST_DURATION", "10s")
	warmup := envOrDefault("CLUSTER_DIST_WARMUP", "2s")
	conns := envOrDefault("CLUSTER_DIST_CONNS", "256")
	workers := envOrDefault("CLUSTER_DIST_WORKERS", "0")
	h2 := envOrDefault("CLUSTER_DIST_H2", "false")
	h2cUpgrade := envOrDefault("CLUSTER_DIST_H2C_UPGRADE", "false")

	if target != "msa2-server" && target != "msr1" {
		return fmt.Errorf("CLUSTER_DIST_TARGET must be msa2-server or msr1 (got %q)", target)
	}

	bins, err := stageBinaries()
	if err != nil {
		return err
	}
	defer cleanupStaging(bins)

	fmt.Printf("\n=== Cluster distributed bench ===\n")
	fmt.Printf("  bench target: %s (server runs here)\n", target)
	fmt.Printf("  loadgen host: msa2-client\n")
	fmt.Printf("  server:       %s\n", serverName)
	fmt.Printf("  url:          http://<%s>:%s%s\n", target, port, urlPath)
	fmt.Printf("  duration:     %s (warmup %s)\n", duration, warmup)
	fmt.Printf("  connections:  %s, workers=%s, h2=%s\n", conns, workers, h2)
	fmt.Printf("  results:      %s\n\n", bins.resultsLocal)

	args := []string{
		"-i", "inventory.yml", clusterDistributedPlaybook,
		"--extra-vars", "bench_target=" + target,
		"--extra-vars", "bench_server_name=" + serverName,
		"--extra-vars", "bench_port=" + port,
		"--extra-vars", "bench_url_path=" + urlPath,
		"--extra-vars", "bench_duration=" + duration,
		"--extra-vars", "bench_warmup=" + warmup,
		"--extra-vars", "bench_connections=" + conns,
		"--extra-vars", "bench_workers=" + workers,
		"--extra-vars", "bench_h2=" + h2,
		"--extra-vars", "bench_h2c_upgrade=" + h2cUpgrade,
		"--extra-vars", "server_binary_amd64=" + bins.serverAmd64,
		"--extra-vars", "server_binary_arm64=" + bins.serverArm64,
		"--extra-vars", "loadgen_binary_amd64=" + bins.loadgenAmd64,
		"--extra-vars", "results_local_dir=" + bins.resultsLocal,
	}
	cmd := exec.Command("ansible-playbook", args...)
	cmd.Dir = clusterAnsibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cluster distributed bench failed: %w", err)
	}

	fmt.Printf("\n=== Distributed bench complete. Results in %s ===\n", bins.resultsLocal)
	return nil
}

// ClusterDistributedBenchParallel runs [ClusterDistributedBench] against
// both bench targets (msa2-server and msr1) concurrently from a single
// msa2-client load source. Each target gets its own subprocess and its
// own results directory, so the two streams of bench data are easy to
// diff post-run. Useful as the merge gate for v1.4.x release branches:
// catches per-target regressions that single-target runs miss when the
// regression is symmetric across the matrix.
//
// Honours every CLUSTER_DIST_* env knob from [ClusterDistributedBench]
// — they apply identically to both target subprocesses. The combined
// run fails if either target subprocess fails.
func ClusterDistributedBenchParallel() error {
	if err := requireAnsible(); err != nil {
		return err
	}
	targets := []string{"msa2-server", "msr1"}
	type runResult struct {
		target string
		err    error
	}
	results := make(chan runResult, len(targets))
	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func(tgt string) {
			defer wg.Done()
			cmd := exec.Command("mage", "ClusterDistributedBench")
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			env := append([]string{}, os.Environ()...)
			env = append(env, "CLUSTER_DIST_TARGET="+tgt)
			cmd.Env = env
			results <- runResult{target: tgt, err: cmd.Run()}
		}(target)
	}
	wg.Wait()
	close(results)
	var failed []string
	for r := range results {
		if r.err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", r.target, r.err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("parallel cluster distributed bench: %s", strings.Join(failed, "; "))
	}
	fmt.Println("\n=== Parallel cluster distributed bench complete on both targets ===")
	return nil
}

// ClusterGoGate stages Go and the celeris source on one or more
// cluster nodes (in parallel when multiple), runs one or more mage
// targets there, fetches each host's log, and tears everything down.
// Used for linux-only or port-bound gates that the dev Mac cannot run
// locally (FullCompliance, MatrixBenchStrict).
//
// Pristine: every artifact lives under {{ bench_root }} on each node;
// the always-block rm -rf's it. The playbook fails the run if anything
// leaks into ~ on the node.
//
// Knobs (env):
//
//	CLUSTER_GO_TARGET     mage targets, space-separated  (required)
//	CLUSTER_GO_HOSTS      comma-separated host list      (default: bench targets, msa2-server,msr1)
//	CLUSTER_GO_HOST       legacy alias for single host
//	CLUSTER_GO_VERSION    Go version to stage            (default: 1.26.3)
//	CLUSTER_GO_TIMEOUT    seconds per host               (default: 28800 = 8h)
//
// When run on more than one host the per-host gate logs land in
// results/<ts>-go-gate-<host>/gate.log and the overall mage target
// fails if any host fails (each is reported with its own error).
func ClusterGoGate() error {
	if err := requireAnsible(); err != nil {
		return err
	}
	target := os.Getenv("CLUSTER_GO_TARGET")
	if target == "" {
		return fmt.Errorf("CLUSTER_GO_TARGET is required (e.g. \"FullCompliance\")")
	}
	// Resolve host list. CLUSTER_GO_HOSTS wins (comma-separated);
	// CLUSTER_GO_HOST is the legacy single-host alias; default is the
	// bench-target group (msa2-server, msr1) so MatrixBenchStrict and
	// kin run on both arch'd nodes by default.
	hostsRaw := os.Getenv("CLUSTER_GO_HOSTS")
	if hostsRaw == "" {
		hostsRaw = os.Getenv("CLUSTER_GO_HOST")
	}
	if hostsRaw == "" {
		hostsRaw = "msa2-server,msr1"
	}
	var hosts []string
	for _, h := range strings.Split(hostsRaw, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	goVer := envOrDefault("CLUSTER_GO_VERSION", "1.26.3")
	timeoutSec := envOrDefault("CLUSTER_GO_TIMEOUT", "28800")

	// Shared staging dir for the multi-host run: Go tarballs (both
	// arches) + source tarball + extras yaml live here once, are
	// pushed N times to the N target hosts.
	stagingDir, err := os.MkdirTemp("", "celeris-go-gate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stagingDir)

	goAmd64 := filepath.Join(stagingDir, "go.linux-amd64.tar.gz")
	goArm64 := filepath.Join(stagingDir, "go.linux-arm64.tar.gz")
	if err := downloadGoTarball(goVer, "amd64", goAmd64); err != nil {
		return fmt.Errorf("download Go %s/amd64: %w", goVer, err)
	}
	if err := downloadGoTarball(goVer, "arm64", goArm64); err != nil {
		return fmt.Errorf("download Go %s/arm64: %w", goVer, err)
	}

	srcTarball := filepath.Join(stagingDir, "source.tar.gz")
	if err := buildSourceTarball(srcTarball); err != nil {
		return fmt.Errorf("build source tarball: %w", err)
	}

	// Sub-second + pid in the timestamp so back-to-back invocations
	// (e.g. a CI job that runs FullCompliance then MatrixBenchStrict)
	// land in distinct results dirs even when launched within the
	// same wall-clock second.
	ts := fmt.Sprintf("%s-p%d", time.Now().UTC().Format("20060102-150405.000"), os.Getpid())

	fmt.Printf("\n=== Cluster go-gate ===\n")
	fmt.Printf("  hosts:          %s\n", strings.Join(hosts, ", "))
	fmt.Printf("  mage target(s): %s\n", target)
	fmt.Printf("  Go version:     %s\n", goVer)
	fmt.Printf("  timeout/host:   %s s\n\n", timeoutSec)

	// Forward env vars the user set locally to the remote shell. We
	// render them as `export k=q` lines and pass the resulting string
	// as an extra-var the playbook embeds into its shell command —
	// simpler than a Jinja loop over a dict (--extra-vars values are
	// always strings, so dict-shaped gate_env trips object/items errors).
	//
	// Forwarded prefixes: project knobs (PERFMATRIX_, FUZZ_, SOAK_,
	// BENCHCMP_) plus the standard Go runtime tuning vars a developer
	// would expect to influence the gate (GOFLAGS, GOEXPERIMENT,
	// GODEBUG, GOTRACEBACK, GORACE, GOMAXPROCS).
	var envExports strings.Builder
	forwardPrefixes := []string{
		"PERFMATRIX_", "FUZZ_", "SOAK_", "BENCHCMP_",
		"GOFLAGS", "GOEXPERIMENT", "GODEBUG",
		"GOTRACEBACK", "GORACE", "GOMAXPROCS",
	}
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		k := kv[:eq]
		for _, p := range forwardPrefixes {
			if k == p || strings.HasPrefix(k, p+"_") || (strings.HasSuffix(p, "_") && strings.HasPrefix(k, p)) {
				fmt.Fprintf(&envExports, "export %s=%s\n", k, shellQuote(kv[eq+1:]))
				break
			}
		}
	}

	// Multi-line strings (the rendered exports block) do not survive a
	// CLI --extra-vars k=v reliably — write the vars to a YAML file
	// alongside the staging dir and pass via @path. Single-line vars
	// stay on the CLI for clarity.
	extrasYAML := filepath.Join(stagingDir, "extras.yml")
	yamlF, err := os.Create(extrasYAML)
	if err != nil {
		return fmt.Errorf("create extras.yml: %w", err)
	}
	fmt.Fprintln(yamlF, "---")
	fmt.Fprintln(yamlF, "gate_env_exports: |")
	for _, line := range strings.Split(strings.TrimRight(envExports.String(), "\n"), "\n") {
		fmt.Fprintln(yamlF, "  "+line)
	}
	yamlF.Close()

	// Fan out across hosts in parallel. Each host gets its own
	// per-host results dir + ansible-playbook subprocess; the shared
	// staging dir holds the inputs (Go tarballs, source, extras).
	type hostResult struct {
		host       string
		resultsDir string
		err        error
	}
	results := make(chan hostResult, len(hosts))
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(host string) {
			defer wg.Done()
			resultsDir, err := filepath.Abs(filepath.Join("results", ts+"-go-gate-"+host))
			if err != nil {
				results <- hostResult{host: host, err: err}
				return
			}
			if err := os.MkdirAll(resultsDir, 0o755); err != nil {
				results <- hostResult{host: host, err: err}
				return
			}
			args := []string{
				"-i", "inventory.yml", clusterGoGatePlaybook,
				"--extra-vars", "gate_target_host=" + host,
				"--extra-vars", "gate_mage_targets=" + target,
				"--extra-vars", "go_tarball_amd64=" + goAmd64,
				"--extra-vars", "go_tarball_arm64=" + goArm64,
				"--extra-vars", "source_tarball_local=" + srcTarball,
				"--extra-vars", "results_local_dir=" + resultsDir,
				"--extra-vars", "gate_timeout_seconds=" + timeoutSec,
				"--extra-vars", "@" + extrasYAML,
			}
			cmd := exec.Command("ansible-playbook", args...)
			cmd.Dir = clusterAnsibleDir
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			results <- hostResult{host: host, resultsDir: resultsDir, err: cmd.Run()}
		}(h)
	}
	wg.Wait()
	close(results)
	var failed []string
	for r := range results {
		if r.err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", r.host, r.err))
			fmt.Printf("=== %s: FAILED — log %s/gate.log ===\n", r.host, r.resultsDir)
		} else {
			fmt.Printf("=== %s: PASSED — log %s/gate.log ===\n", r.host, r.resultsDir)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("cluster go-gate failed on %d host(s): %s", len(failed), strings.Join(failed, "; "))
	}
	fmt.Printf("\n=== Cluster go-gate complete on %d host(s) ===\n", len(hosts))
	return nil
}

// shellQuote wraps s in single quotes safely for /bin/sh, so a value
// containing spaces or shell metacharacters survives a literal `eval`
// or `export` line. Embedded single quotes are split-and-re-escaped.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// downloadGoTarball fetches go<ver>.linux-<arch>.tar.gz from go.dev/dl
// into dst. Skips the download if dst already exists with non-zero size
// (handy when iterating on the playbook). arch is "amd64" or "arm64".
func downloadGoTarball(version, arch, dst string) error {
	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		return nil
	}
	url := "https://go.dev/dl/go" + version + ".linux-" + arch + ".tar.gz"
	cmd := exec.Command("curl", "-fsSL", "-o", dst, url)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// buildSourceTarball runs `git archive HEAD | gzip > dst` to capture the
// current branch state. Tracked files only — no /vendor, no .git, no
// local artifacts. Identical to what dependabot or the GitHub PR diff
// observes.
func buildSourceTarball(dst string) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	gitArchive := exec.Command("git", "archive", "--format=tar.gz", "HEAD")
	gitArchive.Stdout = out
	gitArchive.Stderr = os.Stderr
	return gitArchive.Run()
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
	serverAmd64  string
	serverArm64  string
	loadgenAmd64 string
	resultsLocal string
}

// stageBinaries cross-compiles the perfmatrix runner + server for
// both archs (linux/amd64 + linux/arm64) and the loadgen CLI for
// linux/amd64 (msa2-client only). Prepares the local results landing
// directory. Temp dir is removed by cleanupStaging.
func stageBinaries() (*clusterBins, error) {
	stagingDir, err := os.MkdirTemp("", "celeris-cluster-stage-")
	if err != nil {
		return nil, err
	}
	bins := &clusterBins{
		stagingDir:   stagingDir,
		runnerAmd64:  filepath.Join(stagingDir, "runner-amd64"),
		runnerArm64:  filepath.Join(stagingDir, "runner-arm64"),
		serverAmd64:  filepath.Join(stagingDir, "server-amd64"),
		serverArm64:  filepath.Join(stagingDir, "server-arm64"),
		loadgenAmd64: filepath.Join(stagingDir, "loadgen-amd64"),
	}

	// Time + PID suffix so two ClusterDistributedBench subprocesses
	// kicked off within the same second from ClusterDistributedBenchParallel
	// land in different result dirs.
	ts := time.Now().UTC().Format("20060102-150405.000")
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	bins.resultsLocal = filepath.Join(cwd, "results", fmt.Sprintf("%s-p%d-cluster", ts, os.Getpid()))
	if err := os.MkdirAll(bins.resultsLocal, 0o755); err != nil {
		return nil, err
	}

	type buildJob struct {
		label string
		pkg   string
		out   string
		arch  string
	}
	jobs := []buildJob{
		{"runner linux/amd64", runnerPkgRel, bins.runnerAmd64, "amd64"},
		{"runner linux/arm64", runnerPkgRel, bins.runnerArm64, "arm64"},
		{"server linux/amd64", serverPkgRel, bins.serverAmd64, "amd64"},
		{"server linux/arm64", serverPkgRel, bins.serverArm64, "arm64"},
	}
	for _, j := range jobs {
		fmt.Printf("Cross-compiling %s...\n", j.label)
		if err := crossCompileInDir(perfmatrixModuleDir, j.pkg, j.out, j.arch); err != nil {
			return nil, fmt.Errorf("cross-compile %s: %w", j.label, err)
		}
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
	gomod := "module loadgen-builder\n\ngo 1.26.3\n\nrequire github.com/goceleris/loadgen latest\n"
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
