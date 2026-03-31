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
// Each config specifies the engine, protocol, and the external
// load tool to use (wrk for H1, h2load for H2).
var benchConfigs = []struct {
	engine   string
	protocol string
	loadTool string // "wrk" or "h2load"
}{
	// io_uring — H1 + H2
	{"iouring", "h1", "wrk"},
	{"iouring", "h2", "h2load"},
	// epoll — H1 + H2
	{"epoll", "h1", "wrk"},
	{"epoll", "h2", "h2load"},
	// std — control group
	{"std", "h1", "wrk"},
	{"std", "h2", "h2load"},
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
func buildOneBenchRun(serverBin, engine, protocol, loadTool string) string {
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
		fmt.Sprintf("echo '>>> CONFIG: %s-%s (%s)'", engine, protocol, loadTool),
		// Start server with output to log file.
		fmt.Sprintf("sudo taskset -c 0-3 env ENGINE=%s PROTOCOL=%s PORT=18080 prlimit --memlock=unlimited %s > /tmp/bench/server.log 2>&1 &",
			engine, protocol, serverBin),
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
		parts = append(parts, buildOneBenchRun(serverBin, cfg.engine, cfg.protocol, cfg.loadTool))
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
		name := fmt.Sprintf("%s-%s", cfg.engine, cfg.protocol)
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
	for _, proto := range []string{"h1", "h2"} {
		iouName := fmt.Sprintf("iouring-%s", proto)
		epoName := fmt.Sprintf("epoll-%s", proto)
		iouRPS := medianFloat64(branchVals[iouName])
		epoRPS := medianFloat64(branchVals[epoName])
		if epoRPS > 0 {
			adv := (iouRPS - epoRPS) / epoRPS * 100
			sb.WriteString(fmt.Sprintf("  %-25s: io_uring %8.0f  epoll %8.0f  %+.1f%%\n",
				proto, iouRPS, epoRPS, adv))
		}
	}

	// Add H2 vs H1 comparison.
	sb.WriteString("\n--- H2 vs H1 (branch) ---\n")
	for _, eng := range []string{"iouring", "epoll", "std"} {
		h1Name := fmt.Sprintf("%s-h1", eng)
		h2Name := fmt.Sprintf("%s-h2", eng)
		h1RPS := medianFloat64(branchVals[h1Name])
		h2RPS := medianFloat64(branchVals[h2Name])
		if h1RPS > 0 && h2RPS > 0 {
			ratio := h2RPS / h1RPS * 100
			sb.WriteString(fmt.Sprintf("  %-25s: H2 %8.0f  H1 %8.0f  H2/H1 = %.0f%%\n",
				eng, h2RPS, h1RPS, ratio))
		}
	}

	return sb.String()
}

// benchConfigs defines all engine×protocol combinations for reporting.
var splitBenchConfigs = []struct {
	engine   string
	protocol string
	loadTool string
}{
	{"iouring", "h1", "wrk"},
	{"iouring", "h2", "h2load"},
	{"epoll", "h1", "wrk"},
	{"epoll", "h2", "h2load"},
	{"std", "h1", "wrk"},
	{"std", "h2", "h2load"},
}

// generateThreeWayReport generates a three-way comparison table: main vs savepoint vs current.
// This enables tracking both total improvement (vs main) and incremental improvement (vs savepoint).
func generateThreeWayReport(branchName string, mainRuns, savepointRuns, currentRuns []map[string]float64) string {
	allConfigs := make(map[string]bool)
	mainVals := make(map[string][]float64)
	saveVals := make(map[string][]float64)
	curVals := make(map[string][]float64)
	for _, run := range mainRuns {
		for k, v := range run {
			allConfigs[k] = true
			mainVals[k] = append(mainVals[k], v)
		}
	}
	for _, run := range savepointRuns {
		for k, v := range run {
			allConfigs[k] = true
			saveVals[k] = append(saveVals[k], v)
		}
	}
	for _, run := range currentRuns {
		for k, v := range run {
			allConfigs[k] = true
			curVals[k] = append(curVals[k], v)
		}
	}

	var configs []string
	for _, cfg := range splitBenchConfigs {
		name := fmt.Sprintf("%s-%s", cfg.engine, cfg.protocol)
		if allConfigs[name] {
			configs = append(configs, name)
		}
	}

	var sb strings.Builder
	sb.WriteString("Celeris Three-Way Benchmark Report (wrk + h2load)\n")
	sb.WriteString(fmt.Sprintf("Branch: %s — current vs savepoint (HEAD) vs main\n", branchName))
	sb.WriteString(fmt.Sprintf("Date: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("Rounds: %d per binary (interleaved)\n", len(mainRuns)))
	sb.WriteString(fmt.Sprintf("H1 load: wrk -t4 -c16384 -d%ds\n", loadDuration))
	sb.WriteString(fmt.Sprintf("H2 load: h2load -c128 -m128 -t4 -D%d (16384 concurrent streams)\n", loadDuration))
	sb.WriteString("Server: all 8 CPUs (no taskset), separate client machine\n\n")

	sb.WriteString(fmt.Sprintf("%-28s | %11s | %11s | %11s | %9s | %9s\n",
		"Config", "main (med)", "save (med)", "cur (med)", "vs main", "vs save"))
	sb.WriteString(strings.Repeat("-", 100) + "\n")

	improved, neutral, regressed := 0, 0, 0
	var totalDeltaMain, totalDeltaSave float64

	for _, name := range configs {
		mRPS := medianFloat64(mainVals[name])
		sRPS := medianFloat64(saveVals[name])
		cRPS := medianFloat64(curVals[name])

		deltaMain, deltaSave := 0.0, 0.0
		deltaMainStr, deltaSaveStr := "N/A", "N/A"
		if mRPS > 0 {
			deltaMain = (cRPS - mRPS) / mRPS * 100
			deltaMainStr = fmt.Sprintf("%+.1f%%", deltaMain)
		}
		if sRPS > 0 {
			deltaSave = (cRPS - sRPS) / sRPS * 100
			deltaSaveStr = fmt.Sprintf("%+.1f%%", deltaSave)
		}

		sb.WriteString(fmt.Sprintf("%-28s | %11.0f | %11.0f | %11.0f | %9s | %9s\n",
			name, mRPS, sRPS, cRPS, deltaMainStr, deltaSaveStr))

		if math.Abs(deltaMain) < 2.0 {
			neutral++
		} else if deltaMain > 0 {
			improved++
		} else {
			regressed++
		}
		totalDeltaMain += deltaMain
		totalDeltaSave += deltaSave
	}

	sb.WriteString(strings.Repeat("-", 100) + "\n")
	n := len(configs)
	avgMain, avgSave := 0.0, 0.0
	if n > 0 {
		avgMain = totalDeltaMain / float64(n)
		avgSave = totalDeltaSave / float64(n)
	}
	sb.WriteString(fmt.Sprintf("\nSummary vs main: %d improved, %d neutral, %d regressed (avg: %+.1f%%)\n",
		improved, neutral, regressed, avgMain))
	sb.WriteString(fmt.Sprintf("Summary vs savepoint: avg delta %+.1f%%\n", avgSave))

	// io_uring vs epoll comparison (current).
	sb.WriteString("\n--- io_uring vs epoll (current) ---\n")
	for _, proto := range []string{"h1", "h2"} {
		iouName := fmt.Sprintf("iouring-%s", proto)
		epoName := fmt.Sprintf("epoll-%s", proto)
		iouRPS := medianFloat64(curVals[iouName])
		epoRPS := medianFloat64(curVals[epoName])
		if epoRPS > 0 {
			adv := (iouRPS - epoRPS) / epoRPS * 100
			sb.WriteString(fmt.Sprintf("  %-25s: io_uring %8.0f  epoll %8.0f  %+.1f%%\n",
				proto, iouRPS, epoRPS, adv))
		}
	}

	// H2 vs H1 comparison (current).
	sb.WriteString("\n--- H2 vs H1 (current) ---\n")
	for _, eng := range []string{"iouring", "epoll", "std"} {
		h1Name := fmt.Sprintf("%s-h1", eng)
		h2Name := fmt.Sprintf("%s-h2", eng)
		h1RPS := medianFloat64(curVals[h1Name])
		h2RPS := medianFloat64(curVals[h2Name])
		if h1RPS > 0 && h2RPS > 0 {
			ratio := h2RPS / h1RPS * 100
			sb.WriteString(fmt.Sprintf("  %-25s: H2 %8.0f  H1 %8.0f  H2/H1 = %.0f%%\n",
				eng, h2RPS, h1RPS, ratio))
		}
	}

	return sb.String()
}

// highConnCounts defines the connection counts for the high-connection benchmark.
var highConnCounts = []int{256, 512, 1024, 2048}

// highConnConfigs defines the two configs compared in the high-connection benchmark.
var highConnConfigs = []struct {
	engine   string
	protocol string
}{
	{"iouring", "h1"},
	{"epoll", "h1"},
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
			fmt.Sprintf("echo '>>> CONFIG: %s-%s (wrk) CONNS=%d'",
				cfg.engine, cfg.protocol, conns),
			fmt.Sprintf("sudo taskset -c 0-3 env ENGINE=%s PROTOCOL=%s PORT=18080 prlimit --memlock=unlimited %s > /tmp/bench/server.log 2>&1 &",
				cfg.engine, cfg.protocol, serverBin),
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
		iouKey := fmt.Sprintf("iouring-h1@%d", conns)
		epoKey := fmt.Sprintf("epoll-h1@%d", conns)
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
