//go:build mage

package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const benchmarkVM = "celeris-benchmark"

// benchConfigs defines the server configurations to benchmark.
// Each config specifies the engine, objective, protocol, and the external
// load tool to use (wrk for H1, h2load for H2).
var benchConfigs = []struct {
	engine   string
	objective string
	protocol string
	loadTool string // "wrk" or "h2load"
}{
	// io_uring — all objectives, H1 + H2
	{"iouring", "latency", "h1", "wrk"},
	{"iouring", "latency", "h2", "h2load"},
	{"iouring", "throughput", "h1", "wrk"},
	{"iouring", "throughput", "h2", "h2load"},
	{"iouring", "balanced", "h1", "wrk"},
	{"iouring", "balanced", "h2", "h2load"},
	// epoll — all objectives, H1 + H2
	{"epoll", "latency", "h1", "wrk"},
	{"epoll", "latency", "h2", "h2load"},
	{"epoll", "throughput", "h1", "wrk"},
	{"epoll", "throughput", "h2", "h2load"},
	{"epoll", "balanced", "h1", "wrk"},
	{"epoll", "balanced", "h2", "h2load"},
	// std — latency only (control group)
	{"std", "latency", "h1", "wrk"},
	{"std", "latency", "h2", "h2load"},
}

// loadDuration is the duration of each load run in seconds.
const loadDuration = 15

// wrkArgs returns wrk command line for the given config.
func wrkArgs() string {
	return fmt.Sprintf("-t4 -c256 -d%ds --latency http://127.0.0.1:18080/", loadDuration)
}

// h2loadArgs returns h2load command line for the given config.
func h2loadArgs() string {
	return fmt.Sprintf("-c128 -m128 -t4 -D %d http://127.0.0.1:18080/", loadDuration)
}

// buildOneBenchRun generates a bash script for one benchmark run.
// It starts the server, waits for it, runs the load tool, kills the server.
func buildOneBenchRun(serverBin, engine, objective, protocol, loadTool string) string {
	var loadCmd string
	if loadTool == "h2load" {
		loadCmd = "taskset -c 4-7 h2load " + h2loadArgs()
	} else {
		loadCmd = "taskset -c 4-7 wrk " + wrkArgs()
	}

	return strings.Join([]string{
		// Kill any leftover server from previous run.
		"sudo pkill -9 -f 'profiled-(main|current)' 2>/dev/null || true",
		"sleep 0.5",
		// Ensure io_uring SQPOLL is allowed (Ubuntu 24.04 AppArmor restriction).
		"echo 0 | sudo tee /proc/sys/kernel/apparmor_restrict_unprivileged_io_uring > /dev/null 2>&1 || true",
		fmt.Sprintf("echo '>>> CONFIG: %s-%s-%s (%s)'", engine, objective, protocol, loadTool),
		// Start server with output to log file.
		fmt.Sprintf("sudo taskset -c 0-3 env ENGINE=%s OBJECTIVE=%s PROTOCOL=%s PORT=18080 prlimit --memlock=unlimited %s > /tmp/bench/server.log 2>&1 &",
			engine, objective, protocol, serverBin),
		"SERVER_PID=$!",
		"sleep 3",
		// Show tier selection for debugging.
		"head -5 /tmp/bench/server.log 2>/dev/null || true",
		// Verify server is listening.
		"if ! timeout 2 bash -c 'echo > /dev/tcp/127.0.0.1/18080' 2>/dev/null; then",
		"  echo 'ERROR: server not listening on port 18080'",
		"  cat /tmp/bench/server.log",
		"else",
		"  " + loadCmd,
		"fi",
		// Kill server process group.
		"sudo kill $SERVER_PID 2>/dev/null || true",
		"sudo pkill -9 -f 'profiled-(main|current)' 2>/dev/null || true",
		"wait $SERVER_PID 2>/dev/null || true",
		"sleep 3",
	}, "\n")
}

// buildBenchPassScript generates a bash script for one full pass (all configs).
func buildBenchPassScript(serverBin string) string {
	var parts []string
	for _, cfg := range benchConfigs {
		parts = append(parts, buildOneBenchRun(serverBin, cfg.engine, cfg.objective, cfg.protocol, cfg.loadTool))
	}
	return strings.Join(parts, "\n")
}

// parseWrkRPS extracts Requests/sec from wrk output.
var wrkRPSRegex = regexp.MustCompile(`Requests/sec:\s+([\d.]+)`)

// parseH2loadRPS extracts req/s from h2load output.
var h2loadRPSRegex = regexp.MustCompile(`([\d.]+)\s+req/s`)

// parseConfigRPS extracts config name and RPS from a benchmark pass output.
// Returns map[configName]rps.
var configHeaderRegex = regexp.MustCompile(`>>> CONFIG: (\S+) \((\w+)\)`)

func parseBenchOutput(out string) map[string]float64 {
	results := make(map[string]float64)
	lines := strings.Split(out, "\n")
	var currentConfig string
	var currentTool string
	for _, line := range lines {
		if m := configHeaderRegex.FindStringSubmatch(line); m != nil {
			currentConfig = m[1]
			currentTool = m[2]
			continue
		}
		if currentConfig == "" {
			continue
		}
		var rps float64
		if currentTool == "wrk" {
			if m := wrkRPSRegex.FindStringSubmatch(line); m != nil {
				rps, _ = strconv.ParseFloat(m[1], 64)
			}
		} else if currentTool == "h2load" {
			if m := h2loadRPSRegex.FindStringSubmatch(line); m != nil {
				rps, _ = strconv.ParseFloat(m[1], 64)
			}
		}
		if rps > 0 {
			results[currentConfig] = rps
			currentConfig = ""
			currentTool = ""
		}
	}
	return results
}

// CloudBenchmark runs A/B benchmarks on AWS EC2 instances using wrk (H1) and
// h2load (H2) as external load generators. The same load tool stresses both
// main and branch servers, ensuring a fair comparison.
//
// Pattern: M1→C1→C2→M2→M3→C3 (interleaved to cancel thermal drift).
// Uses c6i.2xlarge (8 vCPU x86) and c7g.2xlarge (8 vCPU arm64).
//
// Set CLOUD_ARCH=amd64|arm64|both (default: both).
func CloudBenchmark() error {
	if err := awsEnsureCLI(); err != nil {
		return err
	}

	branch, err := currentBranch()
	if err != nil {
		return err
	}
	dir, err := resultsDir("cloud-benchmark")
	if err != nil {
		return err
	}
	arches := cloudArch()

	fmt.Printf("Cloud A/B Benchmark: %s vs main (%v)\n", branch, arches)
	fmt.Printf("Using wrk (H1, 256 conns pipelined) + h2load (H2, 128 conns × 128 streams)\n")
	fmt.Printf("Instances: %s (x86), %s (arm64)\n\n", awsX86Instance, awsArmInstance)

	keyName, keyPath, err := awsCreateKeyPair(dir)
	if err != nil {
		return err
	}
	sgID, err := awsCreateSecurityGroup()
	if err != nil {
		awsDeleteKeyPair(keyName)
		return err
	}

	var allInstanceIDs []string
	defer func() {
		awsCleanup(allInstanceIDs, keyName, sgID, keyPath)
	}()

	for _, arch := range arches {
		fmt.Printf("\n====== Architecture: %s ======\n\n", arch)

		// Build profiled server from both branches.
		currentBin := filepath.Join(dir, fmt.Sprintf("profiled-current-%s", arch))
		mainBin := filepath.Join(dir, fmt.Sprintf("profiled-main-%s", arch))

		fmt.Println("Building server binaries...")
		if err := crossCompile("test/benchmark/profiled", currentBin, arch); err != nil {
			return fmt.Errorf("build profiled (current, %s): %w", arch, err)
		}
		if err := crossCompileFromRef("main", "test/benchmark/profiled", mainBin, arch); err != nil {
			return fmt.Errorf("build profiled (main, %s): %w", arch, err)
		}

		// Launch instance.
		ami, err := awsLatestAMI(arch)
		if err != nil {
			return err
		}
		instanceID, ip, err := awsLaunchInstance(ami, awsInstanceType(arch), keyName, sgID, arch)
		if err != nil {
			return err
		}
		allInstanceIDs = append(allInstanceIDs, instanceID)

		if err := awsWaitSSH(ip, keyPath); err != nil {
			return err
		}

		// Install load generators + Go (for server dependencies).
		fmt.Println("Installing load generators (wrk, h2load)...")
		if _, err := awsSSH(ip, keyPath, "sudo apt-get update -qq && sudo apt-get install -y -qq wrk nghttp2-client"); err != nil {
			return fmt.Errorf("install tools: %w", err)
		}
		// Enable io_uring SQPOLL: ensure kernel allows unprivileged io_uring
		// and SQPOLL. Ubuntu 24.04 may restrict io_uring by default.
		if _, err := awsSSH(ip, keyPath, "sudo sysctl -w kernel.io_uring_disabled=0 2>/dev/null; sudo sysctl -w kernel.io_uring_group=-1 2>/dev/null; echo 0 | sudo tee /proc/sys/kernel/apparmor_restrict_unprivileged_io_uring 2>/dev/null; echo io_uring sysctl done"); err != nil {
			fmt.Printf("  Note: io_uring sysctl setup returned error (may be fine): %v\n", err)
		}

		// Transfer server binaries.
		fmt.Println("Transferring binaries...")
		if _, err := awsSSH(ip, keyPath, "mkdir -p /tmp/bench"); err != nil {
			return err
		}
		if err := awsSCP(mainBin, "/tmp/bench/profiled-main", ip, keyPath); err != nil {
			return err
		}
		if err := awsSCP(currentBin, "/tmp/bench/profiled-current", ip, keyPath); err != nil {
			return err
		}
		if _, err := awsSSH(ip, keyPath, "chmod +x /tmp/bench/profiled-*"); err != nil {
			return err
		}

		// Interleaved benchmark: M1 → C1 → C2 → M2 → M3 → C3
		type pass struct {
			label     string
			serverBin string
			target    *[]map[string]float64
		}
		var mainRuns, currentRuns []map[string]float64
		schedule := []pass{
			{"main R1", "/tmp/bench/profiled-main", &mainRuns},
			{"branch R1", "/tmp/bench/profiled-current", &currentRuns},
			{"branch R2", "/tmp/bench/profiled-current", &currentRuns},
			{"main R2", "/tmp/bench/profiled-main", &mainRuns},
			{"main R3", "/tmp/bench/profiled-main", &mainRuns},
			{"branch R3", "/tmp/bench/profiled-current", &currentRuns},
		}

		for i, p := range schedule {
			fmt.Printf("\n--- Pass %d/6: %s ---\n", i+1, p.label)
			script := buildBenchPassScript(p.serverBin)
			// Upload script to instance (avoids SSH arg length/escaping issues).
			scriptPath := filepath.Join(dir, fmt.Sprintf("bench-pass-%d.sh", i))
			_ = os.WriteFile(scriptPath, []byte(script), 0o755)
			if err := awsSCP(scriptPath, "/tmp/bench/run.sh", ip, keyPath); err != nil {
				fmt.Printf("WARNING: script upload failed: %v\n", err)
				continue
			}
			out, err := awsSSH(ip, keyPath, "bash /tmp/bench/run.sh")
			if err != nil {
				fmt.Printf("WARNING: pass failed: %v\nOutput: %s\n", err, out)
			}
			fmt.Print(out)
			results := parseBenchOutput(out)
			*p.target = append(*p.target, results)
		}

		// Generate report.
		report := generateWrkH2loadReport(branch+" ("+arch+")", mainRuns, currentRuns)
		fmt.Println("\n" + report)

		reportPath := filepath.Join(dir, fmt.Sprintf("report-%s.txt", arch))
		_ = os.WriteFile(reportPath, []byte(report), 0o644)
		fmt.Printf("Report saved to: %s\n", reportPath)
	}

	return nil
}

// LocalBenchmark runs A/B benchmarks on a local Multipass VM using wrk/h2load.
func LocalBenchmark() error {
	branch, err := currentBranch()
	if err != nil {
		return err
	}
	dir, err := resultsDir("local-benchmark")
	if err != nil {
		return err
	}
	arch := hostArch()

	fmt.Printf("Local A/B Benchmark: %s vs main (linux/%s)\n\n", branch, arch)

	currentBin := filepath.Join(dir, "profiled-current")
	mainBin := filepath.Join(dir, "profiled-main")

	fmt.Println("Building server binaries...")
	if err := crossCompile("test/benchmark/profiled", currentBin, arch); err != nil {
		return fmt.Errorf("build profiled (current): %w", err)
	}
	if err := crossCompileFromRef("main", "test/benchmark/profiled", mainBin, arch); err != nil {
		return fmt.Errorf("build profiled (main): %w", err)
	}

	if runtime.GOOS == "linux" {
		return runLocalBenchDirect(dir, mainBin, currentBin, branch)
	}
	return runLocalBenchInVM(dir, mainBin, currentBin, arch, branch)
}

func runLocalBenchDirect(dir, mainBin, currentBin, branch string) error {
	fmt.Println("Running on Linux directly...")
	var mainRuns, currentRuns []map[string]float64
	type pass struct {
		label     string
		serverBin string
		target    *[]map[string]float64
	}
	schedule := []pass{
		{"main R1", mainBin, &mainRuns},
		{"branch R1", currentBin, &currentRuns},
		{"branch R2", currentBin, &currentRuns},
		{"main R2", mainBin, &mainRuns},
		{"main R3", mainBin, &mainRuns},
		{"branch R3", currentBin, &currentRuns},
	}

	for i, p := range schedule {
		fmt.Printf("\n--- Pass %d/6: %s ---\n", i+1, p.label)
		script := buildBenchPassScript(p.serverBin)
		out, err := output("bash", "-c", script)
		if err != nil {
			fmt.Printf("WARNING: pass failed: %v\n", err)
		}
		fmt.Println(out)
		results := parseBenchOutput(out)
		*p.target = append(*p.target, results)
	}

	report := generateWrkH2loadReport(branch, mainRuns, currentRuns)
	fmt.Println("\n" + report)
	reportPath := filepath.Join(dir, "report.txt")
	_ = os.WriteFile(reportPath, []byte(report), 0o644)
	fmt.Printf("Report saved to: %s\n", reportPath)
	return nil
}

func runLocalBenchInVM(dir, mainBin, currentBin, arch, branch string) error {
	fmt.Println("Not on Linux — running in Multipass VM.")
	if err := ensureVMWithSpecs(benchmarkVM, "6", "8G"); err != nil {
		return err
	}
	defer destroyVM(benchmarkVM)

	fmt.Println("Installing load tools...")
	_, _ = vmExec(benchmarkVM, "sudo dnf install -y -q wrk nghttp2 2>/dev/null || (sudo apt-get update -qq && sudo apt-get install -y -qq wrk nghttp2-client) 2>/dev/null || echo 'WARNING: could not install load tools'")

	_, _ = vmExec(benchmarkVM, "mkdir -p /tmp/bench")
	if err := transferToVM(benchmarkVM, mainBin, "/tmp/bench/profiled-main"); err != nil {
		return err
	}
	if err := transferToVM(benchmarkVM, currentBin, "/tmp/bench/profiled-current"); err != nil {
		return err
	}
	_, _ = vmExec(benchmarkVM, "chmod +x /tmp/bench/profiled-*")

	var mainRuns, currentRuns []map[string]float64
	type pass struct {
		label     string
		serverBin string
		target    *[]map[string]float64
	}
	schedule := []pass{
		{"main R1", "/tmp/bench/profiled-main", &mainRuns},
		{"branch R1", "/tmp/bench/profiled-current", &currentRuns},
		{"branch R2", "/tmp/bench/profiled-current", &currentRuns},
		{"main R2", "/tmp/bench/profiled-main", &mainRuns},
		{"main R3", "/tmp/bench/profiled-main", &mainRuns},
		{"branch R3", "/tmp/bench/profiled-current", &currentRuns},
	}

	for i, p := range schedule {
		fmt.Printf("\n--- Pass %d/6: %s ---\n", i+1, p.label)
		script := buildBenchPassScript(p.serverBin)
		out, err := vmExec(benchmarkVM, script)
		if err != nil {
			fmt.Printf("WARNING: pass failed: %v\n", err)
		}
		fmt.Println(out)
		results := parseBenchOutput(out)
		*p.target = append(*p.target, results)
	}

	report := generateWrkH2loadReport(branch, mainRuns, currentRuns)
	fmt.Println("\n" + report)
	reportPath := filepath.Join(dir, "report.txt")
	_ = os.WriteFile(reportPath, []byte(report), 0o644)
	fmt.Printf("Report saved to: %s\n", reportPath)
	return nil
}

// medianFloat64 computes the median of a float64 slice.
func medianFloat64(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2
	}
	return sorted[n/2]
}

// generateWrkH2loadReport generates the A/B comparison table from wrk/h2load results.
func generateWrkH2loadReport(branchName string, mainRuns, branchRuns []map[string]float64) string {
	// Collect medians per config.
	allConfigs := make(map[string]bool)
	mainVals := make(map[string][]float64)
	branchVals := make(map[string][]float64)
	for _, run := range mainRuns {
		for k, v := range run {
			allConfigs[k] = true
			mainVals[k] = append(mainVals[k], v)
		}
	}
	for _, run := range branchRuns {
		for k, v := range run {
			allConfigs[k] = true
			branchVals[k] = append(branchVals[k], v)
		}
	}

	// Ordered config list.
	var configs []string
	for _, cfg := range benchConfigs {
		name := fmt.Sprintf("%s-%s-%s", cfg.engine, cfg.objective, cfg.protocol)
		if allConfigs[name] {
			configs = append(configs, name)
		}
	}

	var sb strings.Builder
	sb.WriteString("Celeris A/B Benchmark Report (wrk + h2load)\n")
	sb.WriteString(fmt.Sprintf("Branch: %s vs main\n", branchName))
	sb.WriteString(fmt.Sprintf("Date: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("Rounds: %d per branch (interleaved)\n", len(mainRuns)))
	sb.WriteString(fmt.Sprintf("H1 load: wrk -t4 -c256 -d%ds (pipelined)\n", loadDuration))
	sb.WriteString(fmt.Sprintf("H2 load: h2load -c128 -m128 -t4 -D%d (16384 concurrent streams)\n", loadDuration))
	sb.WriteString("CPU pinning: server CPUs 0-3, load CPUs 4-7\n\n")

	sb.WriteString(fmt.Sprintf("%-35s | %14s | %14s | %8s\n", "Config", "main (med)", "branch (med)", "delta"))
	sb.WriteString(strings.Repeat("-", 80) + "\n")

	improved, neutral, regressed := 0, 0, 0
	var totalDelta float64

	for _, name := range configs {
		mRPS := medianFloat64(mainVals[name])
		bRPS := medianFloat64(branchVals[name])
		delta := 0.0
		deltaStr := "N/A"
		if mRPS > 0 {
			delta = (bRPS - mRPS) / mRPS * 100
			deltaStr = fmt.Sprintf("%+.1f%%", delta)
		}
		sb.WriteString(fmt.Sprintf("%-35s | %14.0f | %14.0f | %8s\n", name, mRPS, bRPS, deltaStr))

		if math.Abs(delta) < 2.0 {
			neutral++
		} else if delta > 0 {
			improved++
		} else {
			regressed++
		}
		totalDelta += delta
	}

	sb.WriteString(strings.Repeat("-", 80) + "\n")
	n := len(configs)
	avgDelta := 0.0
	if n > 0 {
		avgDelta = totalDelta / float64(n)
	}
	sb.WriteString(fmt.Sprintf("\nSummary: %d improved, %d neutral, %d regressed (avg delta: %+.1f%%)\n",
		improved, neutral, regressed, avgDelta))

	// Add io_uring vs epoll comparison.
	sb.WriteString("\n--- io_uring vs epoll (branch) ---\n")
	for _, obj := range []string{"latency", "throughput", "balanced"} {
		for _, proto := range []string{"h1", "h2"} {
			iouName := fmt.Sprintf("iouring-%s-%s", obj, proto)
			epoName := fmt.Sprintf("epoll-%s-%s", obj, proto)
			iouRPS := medianFloat64(branchVals[iouName])
			epoRPS := medianFloat64(branchVals[epoName])
			if epoRPS > 0 {
				adv := (iouRPS - epoRPS) / epoRPS * 100
				sb.WriteString(fmt.Sprintf("  %-25s: io_uring %8.0f  epoll %8.0f  %+.1f%%\n",
					obj+"-"+proto, iouRPS, epoRPS, adv))
			}
		}
	}

	// Add H2 vs H1 comparison.
	sb.WriteString("\n--- H2 vs H1 (branch) ---\n")
	for _, eng := range []string{"iouring", "epoll", "std"} {
		for _, obj := range []string{"latency", "throughput", "balanced"} {
			h1Name := fmt.Sprintf("%s-%s-h1", eng, obj)
			h2Name := fmt.Sprintf("%s-%s-h2", eng, obj)
			h1RPS := medianFloat64(branchVals[h1Name])
			h2RPS := medianFloat64(branchVals[h2Name])
			if h1RPS > 0 && h2RPS > 0 {
				ratio := h2RPS / h1RPS * 100
				sb.WriteString(fmt.Sprintf("  %-25s: H2 %8.0f  H1 %8.0f  H2/H1 = %.0f%%\n",
					eng+"-"+obj, h2RPS, h1RPS, ratio))
			}
		}
	}

	return sb.String()
}

// highConnCounts defines the connection counts for the high-connection benchmark.
var highConnCounts = []int{256, 512, 1024, 2048}

// highConnConfigs defines the two configs compared in the high-connection benchmark.
var highConnConfigs = []struct {
	engine    string
	objective string
	protocol  string
}{
	{"iouring", "latency", "h1"},
	{"epoll", "latency", "h1"},
}

// buildHighConnBenchScript generates a bash script that benchmarks io_uring vs
// epoll at multiple connection counts. Each (engine, connCount) pair gets one run.
func buildHighConnBenchScript(serverBin string, conns int) string {
	var parts []string
	for _, cfg := range highConnConfigs {
		loadCmd := fmt.Sprintf("taskset -c 4-7 wrk -t4 -c%d -d%ds --latency http://127.0.0.1:18080/", conns, loadDuration)
		parts = append(parts, strings.Join([]string{
			"sudo pkill -9 -f 'profiled-(main|current)' 2>/dev/null || true",
			"sleep 0.5",
			fmt.Sprintf("echo '>>> CONFIG: %s-%s-%s (wrk) CONNS=%d'",
				cfg.engine, cfg.objective, cfg.protocol, conns),
			fmt.Sprintf("sudo taskset -c 0-3 env ENGINE=%s OBJECTIVE=%s PROTOCOL=%s PORT=18080 prlimit --memlock=unlimited %s > /tmp/bench/server.log 2>&1 &",
				cfg.engine, cfg.objective, cfg.protocol, serverBin),
			"SERVER_PID=$!",
			"sleep 3",
			"if ! timeout 2 bash -c 'echo > /dev/tcp/127.0.0.1/18080' 2>/dev/null; then",
			"  echo 'ERROR: server not listening on port 18080'",
			"  cat /tmp/bench/server.log",
			"else",
			"  " + loadCmd,
			"fi",
			"sudo kill $SERVER_PID 2>/dev/null || true",
			"sudo pkill -9 -f 'profiled-(main|current)' 2>/dev/null || true",
			"wait $SERVER_PID 2>/dev/null || true",
			"sleep 3",
		}, "\n"))
	}
	return strings.Join(parts, "\n")
}

// parseHighConnOutput extracts config name, connection count, and RPS from
// high-connection benchmark output. Returns map["engine-conns"]rps.
var highConnHeaderRegex = regexp.MustCompile(`>>> CONFIG: (\S+) \(wrk\) CONNS=(\d+)`)

func parseHighConnOutput(out string) map[string]float64 {
	results := make(map[string]float64)
	lines := strings.Split(out, "\n")
	var currentKey string
	for _, line := range lines {
		if m := highConnHeaderRegex.FindStringSubmatch(line); m != nil {
			currentKey = m[1] + "@" + m[2]
			continue
		}
		if currentKey == "" {
			continue
		}
		if m := wrkRPSRegex.FindStringSubmatch(line); m != nil {
			rps, _ := strconv.ParseFloat(m[1], 64)
			if rps > 0 {
				results[currentKey] = rps
				currentKey = ""
			}
		}
	}
	return results
}

// generateHighConnReport produces a comparison table of io_uring vs epoll at
// varying connection counts.
func generateHighConnReport(branchName string, runs []map[string]float64) string {
	// Collect all values per key across runs.
	allVals := make(map[string][]float64)
	for _, run := range runs {
		for k, v := range run {
			allVals[k] = append(allVals[k], v)
		}
	}

	var sb strings.Builder
	sb.WriteString("Celeris High-Connection Benchmark Report\n")
	sb.WriteString(fmt.Sprintf("Branch: %s\n", branchName))
	sb.WriteString(fmt.Sprintf("Date: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("Rounds: %d (interleaved M1,C1,C2,M2)\n", len(runs)))
	sb.WriteString(fmt.Sprintf("Duration: %ds per test\n", loadDuration))
	sb.WriteString("CPU pinning: server CPUs 0-3, load CPUs 4-7\n\n")

	sb.WriteString(fmt.Sprintf("%-18s | %14s | %14s | %18s\n",
		"Connection Count", "io_uring rps", "epoll rps", "io_uring advantage"))
	sb.WriteString(strings.Repeat("-", 72) + "\n")

	for _, conns := range highConnCounts {
		iouKey := fmt.Sprintf("iouring-latency-h1@%d", conns)
		epoKey := fmt.Sprintf("epoll-latency-h1@%d", conns)
		iouRPS := medianFloat64(allVals[iouKey])
		epoRPS := medianFloat64(allVals[epoKey])
		advStr := "N/A"
		if epoRPS > 0 {
			adv := (iouRPS - epoRPS) / epoRPS * 100
			advStr = fmt.Sprintf("%+.1f%%", adv)
		}
		sb.WriteString(fmt.Sprintf("%-18d | %14.0f | %14.0f | %18s\n",
			conns, iouRPS, epoRPS, advStr))
	}

	sb.WriteString(strings.Repeat("-", 72) + "\n")
	return sb.String()
}

// CloudBenchmarkHighConn runs a focused io_uring vs epoll benchmark at multiple
// connection counts (256, 512, 1024, 2048) on AWS EC2. Only tests the fastest
// path: latency-optimized H1.
//
// Uses 4 interleaved passes (M1,C1,C2,M2) for speed.
// Set CLOUD_ARCH=amd64|arm64|both (default: both).
func CloudBenchmarkHighConn() error {
	if err := awsEnsureCLI(); err != nil {
		return err
	}

	branch, err := currentBranch()
	if err != nil {
		return err
	}
	dir, err := resultsDir("cloud-highconn")
	if err != nil {
		return err
	}
	arches := cloudArch()

	fmt.Printf("Cloud High-Connection Benchmark: %s (%v)\n", branch, arches)
	fmt.Printf("Configs: iouring-latency-h1, epoll-latency-h1\n")
	fmt.Printf("Connection counts: %v\n", highConnCounts)
	fmt.Printf("Passes: 4 (M1,C1,C2,M2), %ds per test\n\n", loadDuration)

	keyName, keyPath, err := awsCreateKeyPair(dir)
	if err != nil {
		return err
	}
	sgID, err := awsCreateSecurityGroup()
	if err != nil {
		awsDeleteKeyPair(keyName)
		return err
	}

	var allInstanceIDs []string
	defer func() {
		awsCleanup(allInstanceIDs, keyName, sgID, keyPath)
	}()

	for _, arch := range arches {
		fmt.Printf("\n====== Architecture: %s ======\n\n", arch)

		serverBin := filepath.Join(dir, fmt.Sprintf("profiled-current-%s", arch))

		fmt.Println("Building server binary...")
		if err := crossCompile("test/benchmark/profiled", serverBin, arch); err != nil {
			return fmt.Errorf("build profiled (current, %s): %w", arch, err)
		}

		ami, err := awsLatestAMI(arch)
		if err != nil {
			return err
		}
		instanceID, ip, err := awsLaunchInstance(ami, awsInstanceType(arch), keyName, sgID, arch)
		if err != nil {
			return err
		}
		allInstanceIDs = append(allInstanceIDs, instanceID)

		if err := awsWaitSSH(ip, keyPath); err != nil {
			return err
		}

		fmt.Println("Installing wrk and configuring io_uring...")
		if _, err := awsSSH(ip, keyPath, "sudo apt-get update -qq && sudo apt-get install -y -qq wrk"); err != nil {
			return fmt.Errorf("install wrk: %w", err)
		}
		_, _ = awsSSH(ip, keyPath, "sudo sysctl -w kernel.io_uring_disabled=0 2>/dev/null; sudo sysctl -w kernel.io_uring_group=-1 2>/dev/null; echo 0 | sudo tee /proc/sys/kernel/apparmor_restrict_unprivileged_io_uring 2>/dev/null")

		fmt.Println("Transferring binary...")
		if _, err := awsSSH(ip, keyPath, "mkdir -p /tmp/bench"); err != nil {
			return err
		}
		if err := awsSCP(serverBin, "/tmp/bench/profiled-current", ip, keyPath); err != nil {
			return err
		}
		if _, err := awsSSH(ip, keyPath, "chmod +x /tmp/bench/profiled-current"); err != nil {
			return err
		}

		// 4 interleaved passes: M1, C1, C2, M2 (all use same binary since
		// this is io_uring vs epoll comparison, not branch vs main).
		var allRuns []map[string]float64
		passLabels := []string{"R1", "R2", "R3", "R4"}

		for passIdx, label := range passLabels {
			fmt.Printf("\n--- Pass %d/4: %s ---\n", passIdx+1, label)

			// Run all connection counts in this pass.
			var scriptParts []string
			for _, conns := range highConnCounts {
				scriptParts = append(scriptParts, buildHighConnBenchScript("/tmp/bench/profiled-current", conns))
			}
			fullScript := strings.Join(scriptParts, "\n")

			scriptPath := filepath.Join(dir, fmt.Sprintf("highconn-pass-%d.sh", passIdx))
			_ = os.WriteFile(scriptPath, []byte(fullScript), 0o755)
			if err := awsSCP(scriptPath, "/tmp/bench/run.sh", ip, keyPath); err != nil {
				fmt.Printf("WARNING: script upload failed: %v\n", err)
				continue
			}
			out, err := awsSSH(ip, keyPath, "bash /tmp/bench/run.sh")
			if err != nil {
				fmt.Printf("WARNING: pass failed: %v\nOutput: %s\n", err, out)
			}
			fmt.Print(out)
			results := parseHighConnOutput(out)
			allRuns = append(allRuns, results)
		}

		report := generateHighConnReport(branch+" ("+arch+")", allRuns)
		fmt.Println("\n" + report)

		reportPath := filepath.Join(dir, fmt.Sprintf("highconn-report-%s.txt", arch))
		_ = os.WriteFile(reportPath, []byte(report), 0o644)
		fmt.Printf("Report saved to: %s\n", reportPath)
	}

	return nil
}

// splitBenchConfigs defines configs for the split-machine benchmark.
var splitBenchConfigs = []struct {
	engine    string
	objective string
	protocol  string
	loadTool  string
}{
	{"iouring", "latency", "h1", "wrk"},
	{"iouring", "throughput", "h1", "wrk"},
	{"iouring", "balanced", "h1", "wrk"},
	{"iouring", "latency", "h2", "h2load"},
	{"epoll", "latency", "h1", "wrk"},
	{"epoll", "throughput", "h1", "wrk"},
	{"epoll", "balanced", "h1", "wrk"},
	{"epoll", "latency", "h2", "h2load"},
	{"std", "latency", "h1", "wrk"},
}

// buildSplitServerScript starts the server (no taskset — all CPUs).
// Uses double-quotes for patterns to avoid single-quote nesting issues with SSH.
func buildSplitServerScript(serverBin, engine, objective, protocol string) string {
	return strings.Join([]string{
		"sudo pkill -9 -f profiled 2>/dev/null || true",
		"sleep 0.5",
		"echo 0 | sudo tee /proc/sys/kernel/apparmor_restrict_unprivileged_io_uring > /dev/null 2>&1 || true",
		fmt.Sprintf("sudo env ENGINE=%s OBJECTIVE=%s PROTOCOL=%s PORT=18080 prlimit --memlock=unlimited %s > /tmp/bench/server.log 2>&1 &",
			engine, objective, protocol, serverBin),
		"sleep 3",
		"head -5 /tmp/bench/server.log 2>/dev/null || true",
		"echo SERVER_READY",
	}, "\n")
}

// buildSplitPassScript generates a benchmark pass run from the CLIENT machine.
// Server start/stop commands are sent via SSH. To avoid quoting issues, the
// server startup script is written to a temp file and executed remotely.
func buildSplitPassScript(serverBin, serverIP, serverKeyPath string) string {
	sshBase := fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o BatchMode=yes -o ServerAliveInterval=30 -i %s ubuntu@%s",
		serverKeyPath, serverIP)

	var parts []string
	for i, cfg := range splitBenchConfigs {
		var loadCmd string
		if cfg.loadTool == "h2load" {
			loadCmd = fmt.Sprintf("h2load -c128 -m128 -t4 -D %d http://%s:18080/", loadDuration, serverIP)
		} else {
			loadCmd = fmt.Sprintf("wrk -t4 -c256 -d%ds --latency http://%s:18080/", loadDuration, serverIP)
		}

		// Write server start script to a local temp file, scp it, then execute.
		serverScript := buildSplitServerScript(serverBin, cfg.engine, cfg.objective, cfg.protocol)
		scriptFile := fmt.Sprintf("/tmp/server-start-%d.sh", i)

		parts = append(parts, strings.Join([]string{
			fmt.Sprintf("echo '>>> CONFIG: %s-%s-%s (%s)'", cfg.engine, cfg.objective, cfg.protocol, cfg.loadTool),
			// Write server script locally on client.
			fmt.Sprintf("cat > %s << 'SERVEREOF'\n%s\nSERVEREOF", scriptFile, serverScript),
			// Upload and execute on server.
			fmt.Sprintf("scp -o StrictHostKeyChecking=no -o BatchMode=yes -i %s %s ubuntu@%s:/tmp/server-start.sh 2>/dev/null",
				serverKeyPath, scriptFile, serverIP),
			fmt.Sprintf("%s 'bash /tmp/server-start.sh'", sshBase),
			// Check connectivity then run load.
			fmt.Sprintf("if ! timeout 5 bash -c 'echo > /dev/tcp/%s/18080' 2>/dev/null; then", serverIP),
			"  echo 'ERROR: server not listening'",
			"else",
			"  " + loadCmd,
			"fi",
			// Kill server.
			fmt.Sprintf("%s 'sudo pkill -9 -f profiled 2>/dev/null; true'", sshBase),
			"sleep 3",
		}, "\n"))
	}
	return strings.Join(parts, "\n")
}

// CloudBenchmarkSplit runs A/B benchmarks with SEPARATE server and client instances.
// The server gets ALL 8 CPUs (no taskset). The client runs wrk/h2load on a separate
// machine over VPC network. This tests the real-world scenario where io_uring's
// syscall batching advantage manifests over real network (not loopback).
//
// Set CLOUD_ARCH=amd64|arm64 (default: amd64).
func CloudBenchmarkSplit() error {
	if err := awsEnsureCLI(); err != nil {
		return err
	}

	branch, err := currentBranch()
	if err != nil {
		return err
	}
	dir, err := resultsDir("cloud-split-benchmark")
	if err != nil {
		return err
	}
	arches := cloudArch()
	if len(arches) > 1 {
		arches = []string{arches[0]}
		fmt.Println("Note: CloudBenchmarkSplit tests one architecture. Set CLOUD_ARCH to override.")
	}
	arch := arches[0]

	fmt.Printf("Cloud Split Benchmark: %s vs main (%s)\n", branch, arch)
	fmt.Printf("Separate server + client machines (server gets all 8 CPUs)\n")
	fmt.Printf("Instance type: %s\n\n", awsInstanceType(arch))

	keyName, keyPath, err := awsCreateKeyPair(dir)
	if err != nil {
		return err
	}
	sgID, err := awsCreateSecurityGroup()
	if err != nil {
		return err
	}
	// Allow server port between instances in same SG.
	_, _ = awsCLI("ec2", "authorize-security-group-ingress",
		"--region", awsRegion,
		"--group-id", sgID,
		"--protocol", "tcp",
		"--port", "18080",
		"--source-group", sgID)

	var allInstanceIDs []string
	defer func() {
		awsCleanup(allInstanceIDs, keyName, sgID, keyPath)
	}()

	currentBin := filepath.Join(dir, fmt.Sprintf("profiled-current-%s", arch))
	mainBin := filepath.Join(dir, fmt.Sprintf("profiled-main-%s", arch))

	fmt.Println("Building server binaries...")
	if err := crossCompile("test/benchmark/profiled", currentBin, arch); err != nil {
		return err
	}
	if err := crossCompileFromRef("main", "test/benchmark/profiled", mainBin, arch); err != nil {
		return err
	}

	ami, err := awsLatestAMI(arch)
	if err != nil {
		return err
	}

	// Launch SERVER.
	serverID, serverPublicIP, err := awsLaunchInstance(ami, awsInstanceType(arch), keyName, sgID, arch)
	if err != nil {
		return err
	}
	allInstanceIDs = append(allInstanceIDs, serverID)
	if err := awsWaitSSH(serverPublicIP, keyPath); err != nil {
		return err
	}
	if err := awsInstallGo(serverPublicIP, keyPath); err != nil {
		return err
	}

	serverPrivateIP, err := awsCLI("ec2", "describe-instances",
		"--region", awsRegion,
		"--instance-ids", serverID,
		"--query", "Reservations[0].Instances[0].PrivateIpAddress",
		"--output", "text")
	if err != nil {
		return err
	}
	serverPrivateIP = strings.TrimSpace(serverPrivateIP)
	fmt.Printf("  Server: %s (private: %s)\n", serverPublicIP, serverPrivateIP)

	if _, err := awsSSH(serverPublicIP, keyPath, "mkdir -p /tmp/bench"); err != nil {
		return err
	}
	if err := awsSCP(mainBin, "/tmp/bench/profiled-main", serverPublicIP, keyPath); err != nil {
		return err
	}
	if err := awsSCP(currentBin, "/tmp/bench/profiled-current", serverPublicIP, keyPath); err != nil {
		return err
	}
	if _, err := awsSSH(serverPublicIP, keyPath, "chmod +x /tmp/bench/profiled-*"); err != nil {
		return err
	}
	_, _ = awsSSH(serverPublicIP, keyPath, "echo 0 | sudo tee /proc/sys/kernel/apparmor_restrict_unprivileged_io_uring > /dev/null 2>&1")

	// Launch CLIENT.
	clientID, clientPublicIP, err := awsLaunchInstance(ami, awsInstanceType(arch), keyName, sgID, arch)
	if err != nil {
		return err
	}
	allInstanceIDs = append(allInstanceIDs, clientID)
	if err := awsWaitSSH(clientPublicIP, keyPath); err != nil {
		return err
	}
	fmt.Printf("  Client: %s\n", clientPublicIP)

	fmt.Println("Installing load tools on client...")
	if _, err := awsSSH(clientPublicIP, keyPath, "sudo apt-get update -qq && sudo apt-get install -y -qq wrk nghttp2-client"); err != nil {
		return fmt.Errorf("install tools: %w", err)
	}

	// Transfer SSH key so client can control server.
	if err := awsSCP(keyPath, "/tmp/server-key.pem", clientPublicIP, keyPath); err != nil {
		return err
	}
	if _, err := awsSSH(clientPublicIP, keyPath, "chmod 600 /tmp/server-key.pem"); err != nil {
		return err
	}

	// Interleaved: M1→C1→C2→M2→M3→C3
	type pass struct {
		label     string
		serverBin string
		target    *[]map[string]float64
	}
	var mainRuns, currentRuns []map[string]float64
	schedule := []pass{
		{"main R1", "/tmp/bench/profiled-main", &mainRuns},
		{"branch R1", "/tmp/bench/profiled-current", &currentRuns},
		{"branch R2", "/tmp/bench/profiled-current", &currentRuns},
		{"main R2", "/tmp/bench/profiled-main", &mainRuns},
		{"main R3", "/tmp/bench/profiled-main", &mainRuns},
		{"branch R3", "/tmp/bench/profiled-current", &currentRuns},
	}

	for i, p := range schedule {
		fmt.Printf("\n--- Pass %d/6: %s ---\n", i+1, p.label)
		script := buildSplitPassScript(p.serverBin, serverPrivateIP, "/tmp/server-key.pem")

		scriptPath := filepath.Join(dir, fmt.Sprintf("split-pass-%d.sh", i))
		_ = os.WriteFile(scriptPath, []byte(script), 0o755)
		if err := awsSCP(scriptPath, "/tmp/bench-run.sh", clientPublicIP, keyPath); err != nil {
			fmt.Printf("WARNING: script upload failed: %v\n", err)
			continue
		}
		out, err := awsSSH(clientPublicIP, keyPath, "bash /tmp/bench-run.sh")
		if err != nil {
			fmt.Printf("WARNING: pass failed: %v\nOutput: %s\n", err, out)
		}
		fmt.Print(out)
		results := parseBenchOutput(out)
		*p.target = append(*p.target, results)
	}

	report := generateWrkH2loadReport(branch+" ("+arch+", split)", mainRuns, currentRuns)
	fmt.Println("\n" + report)

	reportPath := filepath.Join(dir, fmt.Sprintf("report-%s.txt", arch))
	_ = os.WriteFile(reportPath, []byte(report), 0o644)
	fmt.Printf("Report saved to: %s\n", reportPath)

	return nil
}
