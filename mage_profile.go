//go:build mage

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const profileVM = "celeris-profile"

// profileConfigs defines the configs to profile.
// All 9 engine×protocol combinations for comprehensive coverage.
var profileConfigs = []struct {
	engine   string
	protocol string
}{
	{"epoll", "h1"},
	{"epoll", "h2"},
	{"epoll", "hybrid"},
	{"iouring", "h1"},
	{"iouring", "h2"},
	{"iouring", "hybrid"},
	{"adaptive", "h1"},
	{"adaptive", "h2"},
	{"adaptive", "hybrid"},
}

// profileArtifacts lists the files captured per profile run.
var profileArtifacts = []string{
	"cpu.prof", "heap.prof", "allocs.prof", "goroutine.txt", "perf-stat.txt",
}

// LocalProfile captures pprof profiles (CPU, heap, allocs, goroutine) and
// perf stat counters for both main and current branch across key engine
// configurations. On non-Linux hosts, a Multipass VM is created and destroyed.
// Results are saved to results/<timestamp>-local-profile/.
func LocalProfile() error {
	branch, err := currentBranch()
	if err != nil {
		return err
	}
	dir, err := resultsDir("local-profile")
	if err != nil {
		return err
	}
	arch := hostArch()

	fmt.Printf("Profiling: %s vs main (linux/%s)\n\n", branch, arch)

	bins := map[string]string{
		"profiled-current":  filepath.Join(dir, "profiled-current"),
		"profiled-main":     filepath.Join(dir, "profiled-main"),
		"fullstack-current": filepath.Join(dir, "fullstack-current"),
	}

	fmt.Println("Building binaries...")
	if err := crossCompile("test/benchmark/profiled", bins["profiled-current"], arch); err != nil {
		return fmt.Errorf("build profiled (current): %w", err)
	}
	if err := crossCompileFromRef("main", "test/benchmark/profiled", bins["profiled-main"], arch); err != nil {
		return fmt.Errorf("build profiled (main): %w", err)
	}
	if err := crossCompile("test/loadtest/fullstack", bins["fullstack-current"], arch); err != nil {
		return fmt.Errorf("build fullstack (current): %w", err)
	}

	if runtime.GOOS == "linux" {
		return runProfileDirect(dir, bins, branch)
	}
	return runProfileInVM(dir, bins, arch, branch)
}

func runProfileDirect(dir string, bins map[string]string, branch string) error {
	fmt.Println("Running profiling directly on Linux...")
	return runProfileOnTarget(dir, branch, func(version, engine, protocol string) error {
		profiledBin := bins["profiled-main"]
		if version == "current" {
			profiledBin = bins["profiled-current"]
		}
		return profileOneConfig(dir, version, engine, protocol,
			profiledBin, bins["fullstack-current"])
	})
}

func runProfileInVM(dir string, bins map[string]string, arch, branch string) error {
	fmt.Println("Not on Linux — running profiling in Multipass VM.")

	if err := ensureVMWithSpecs(profileVM, "4", "8G"); err != nil {
		return err
	}
	defer destroyVM(profileVM)

	fmt.Println("Installing load tools (wrk, h2load, perf)...")
	_, _ = vmExec(profileVM, "sudo dnf install -y -q wrk nghttp2 linux-tools-$(uname -r) 2>/dev/null || (sudo apt-get update -qq && sudo apt-get install -y -qq wrk nghttp2-client linux-tools-$(uname -r) linux-tools-common curl 2>/dev/null) || true")

	remoteBins := map[string]string{
		"profiled-main":     "/tmp/profile/profiled-main",
		"profiled-current":  "/tmp/profile/profiled-current",
		"fullstack-current": "/tmp/profile/fullstack-current",
	}
	_, _ = vmExec(profileVM, "mkdir -p /tmp/profile /tmp/profile/results")
	for name, localPath := range bins {
		if err := transferToVM(profileVM, localPath, remoteBins[name]); err != nil {
			return fmt.Errorf("transfer %s: %w", name, err)
		}
	}
	_, _ = vmExec(profileVM, "chmod +x /tmp/profile/*")

	return runProfileOnTarget(dir, branch, func(version, engine, protocol string) error {
		profiledBin := remoteBins["profiled-main"]
		if version == "current" {
			profiledBin = remoteBins["profiled-current"]
		}
		loadBin := remoteBins["fullstack-current"]
		tag := fmt.Sprintf("%s-%s", engine, protocol)

		remoteDir := fmt.Sprintf("/tmp/profile/results/%s/%s", tag, version)
		_, _ = vmExec(profileVM, "mkdir -p "+remoteDir)

		script := buildProfileScript(profiledBin, loadBin, engine, protocol, remoteDir)
		fmt.Printf("  Running profile script in VM...\n")
		if _, err := vmExec(profileVM, script); err != nil {
			fmt.Printf("  WARNING: profile capture failed: %v\n", err)
		}

		localDir := filepath.Join(dir, tag, version)
		_ = os.MkdirAll(localDir, 0o755)
		for _, f := range profileArtifacts {
			remotePath := filepath.Join(remoteDir, f)
			localPath := filepath.Join(localDir, f)
			_ = transferFromVM(profileVM, remotePath, localPath)
		}
		return nil
	})
}

// runProfileOnTarget iterates through configs and branches, calling profileFn
// for each. After all captures, it runs automated profile analysis.
func runProfileOnTarget(dir, branch string, profileFn func(version, engine, protocol string) error) error {
	for _, cfg := range profileConfigs {
		tag := fmt.Sprintf("%s-%s", cfg.engine, cfg.protocol)
		for _, version := range []string{"main", "current"} {
			label := "main"
			if version == "current" {
				label = branch
			}
			fmt.Printf("\n--- Profiling: %s %s ---\n", label, tag)
			if err := profileFn(version, cfg.engine, cfg.protocol); err != nil {
				fmt.Printf("  WARNING: %s %s failed: %v\n", version, tag, err)
			}
		}
	}

	fmt.Printf("\n=== Profiles saved to %s ===\n", dir)

	if err := analyzeProfiles(dir); err != nil {
		fmt.Printf("WARNING: profile analysis failed: %v\n", err)
	}

	return nil
}

// profileOneConfig runs one profile capture (Linux direct mode).
func profileOneConfig(dir, version, engine, protocol, profiledBin, loadBin string) error {
	tag := fmt.Sprintf("%s-%s", engine, protocol)
	localDir := filepath.Join(dir, tag, version)
	_ = os.MkdirAll(localDir, 0o755)

	script := buildProfileScript(profiledBin, loadBin, engine, protocol, localDir)
	cmd := exec.Command("bash", "-c", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// buildProfileScript generates a bash script that starts the profiled server,
// applies sustained load via h2load/wrk, captures pprof profiles and perf stat
// counters, and cleans up. Includes diagnostics for empty profile detection.
func buildProfileScript(profiledBin, _ /* loadBin unused */, engine, protocol, outputDir string) string {
	var loadCmd string
	if protocol == "h2" {
		loadCmd = "taskset -c 4-7 h2load -c 128 -m 128 -t 4 -D 25 http://127.0.0.1:18080/ > /dev/null 2>&1 &"
	} else {
		loadCmd = "taskset -c 4-7 wrk -t4 -c256 -d25s http://127.0.0.1:18080/ > /dev/null 2>&1 &"
	}

	return strings.Join([]string{
		"set -e",
		"# Kill any leftover server.",
		"sudo pkill -9 -f profiled 2>/dev/null || true",
		"sleep 1",
		fmt.Sprintf("sudo taskset -c 0-3 env ENGINE=%s PROTOCOL=%s PORT=18080 prlimit --memlock=unlimited %s > /tmp/profiled.log 2>&1 &",
			engine, protocol, profiledBin),
		"SERVER_PID=$!",
		"sleep 2",
		"",
		"# Verify server is listening.",
		"if ! timeout 3 bash -c 'echo > /dev/tcp/127.0.0.1/18080' 2>/dev/null; then",
		"  echo 'ERROR: server not listening on port 18080'",
		"  cat /tmp/profiled.log 2>/dev/null || true",
		"  kill $SERVER_PID 2>/dev/null || true",
		"  exit 1",
		"fi",
		"# Verify pprof endpoint.",
		"if ! timeout 3 bash -c 'echo > /dev/tcp/127.0.0.1/6060' 2>/dev/null; then",
		"  echo 'ERROR: pprof not listening on port 6060'",
		"  cat /tmp/profiled.log 2>/dev/null || true",
		"  kill $SERVER_PID 2>/dev/null || true",
		"  exit 1",
		"fi",
		"echo 'Server and pprof endpoints verified.'",
		"",
		"# Sustained load (25s).",
		loadCmd,
		"LOAD_PID=$!",
		"",
		"# Warmup.",
		"sleep 3",
		"",
		"# Capture perf stat counters (15s, background).",
		"SERVER_REAL_PID=$(pgrep -f profiled | grep -v pgrep | head -1)",
		"if [ -n \"$SERVER_REAL_PID\" ] && command -v perf > /dev/null 2>&1; then",
		fmt.Sprintf("  sudo perf stat -e cycles,instructions,cache-misses,cache-references,context-switches,cpu-migrations,page-faults -p $SERVER_REAL_PID -- sleep 15 > %s/perf-stat.txt 2>&1 &", outputDir),
		"  PERF_PID=$!",
		"  echo \"perf stat started for PID $SERVER_REAL_PID\"",
		"else",
		"  echo 'NOTE: perf stat skipped (perf not installed or server PID not found)'",
		"  PERF_PID=",
		"fi",
		"",
		"# Capture CPU profile (blocks for 20s).",
		"echo 'Capturing CPU profile (20s)...'",
		fmt.Sprintf("HTTP_CODE=$(curl -s -w '%%{http_code}' -o %s/cpu.prof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=20')", outputDir),
		"if [ \"$HTTP_CODE\" != \"200\" ]; then",
		"  echo \"ERROR: CPU profile failed with HTTP $HTTP_CODE\"",
		"else",
		fmt.Sprintf("  CPU_SIZE=$(stat -c %%s %s/cpu.prof 2>/dev/null || stat -f %%z %s/cpu.prof 2>/dev/null || echo 0)", outputDir, outputDir),
		"  echo \"CPU profile: ${CPU_SIZE} bytes\"",
		"  if [ \"$CPU_SIZE\" -lt 1000 ]; then",
		"    echo 'WARNING: CPU profile suspiciously small (< 1KB) — likely empty'",
		"  fi",
		"fi",
		"",
		"# Capture heap, allocs, goroutine.",
		fmt.Sprintf("curl -sf -o %s/heap.prof 'http://127.0.0.1:6060/debug/pprof/heap' || echo 'WARNING: heap profile failed'", outputDir),
		fmt.Sprintf("curl -sf -o %s/allocs.prof 'http://127.0.0.1:6060/debug/pprof/allocs' || echo 'WARNING: allocs profile failed'", outputDir),
		fmt.Sprintf("curl -sf -o %s/goroutine.txt 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1' || echo 'WARNING: goroutine profile failed'", outputDir),
		"",
		"# Wait for perf stat.",
		"if [ -n \"$PERF_PID\" ]; then",
		"  wait $PERF_PID 2>/dev/null || true",
		"  echo '--- perf stat ---'",
		fmt.Sprintf("  cat %s/perf-stat.txt 2>/dev/null || true", outputDir),
		"fi",
		"",
		"# Report artifact sizes.",
		"echo '--- Profile artifacts ---'",
		fmt.Sprintf("ls -la %s/ 2>/dev/null || true", outputDir),
		"",
		"# Cleanup.",
		"kill $LOAD_PID 2>/dev/null || true",
		"kill $SERVER_PID 2>/dev/null || true",
		"sudo pkill -9 -f profiled 2>/dev/null || true",
		"wait $LOAD_PID 2>/dev/null || true",
		"wait $SERVER_PID 2>/dev/null || true",
		"sleep 1",
	}, "\n")
}

func analyzeProfiles(dir string) error {
	fmt.Println("\n=== Automated Profile Analysis ===")

	var report strings.Builder
	report.WriteString("Profile Analysis Report\n")
	report.WriteString(strings.Repeat("=", 60) + "\n\n")

	for _, cfg := range profileConfigs {
		tag := fmt.Sprintf("%s-%s", cfg.engine, cfg.protocol)
		report.WriteString(fmt.Sprintf("Config: %s\n", tag))
		report.WriteString(strings.Repeat("-", 40) + "\n")

		currentCPU := filepath.Join(dir, tag, "current", "cpu.prof")
		mainCPU := filepath.Join(dir, tag, "main", "cpu.prof")

		// Current branch CPU top 20.
		if isValidProfile(currentCPU) {
			report.WriteString("\n[current] CPU Top 20 (cumulative):\n")
			if out, err := output("go", "tool", "pprof", "-top", "-cum", "-nodecount=20", currentCPU); err != nil {
				report.WriteString(fmt.Sprintf("  ERROR: %v\n", err))
			} else {
				report.WriteString(out + "\n")
			}
		} else {
			report.WriteString("\n[current] CPU profile: missing or too small\n")
		}

		// Main branch CPU top 20.
		if isValidProfile(mainCPU) {
			report.WriteString("\n[main] CPU Top 20 (cumulative):\n")
			if out, err := output("go", "tool", "pprof", "-top", "-cum", "-nodecount=20", mainCPU); err != nil {
				report.WriteString(fmt.Sprintf("  ERROR: %v\n", err))
			} else {
				report.WriteString(out + "\n")
			}
		}

		// Diff: current vs main.
		if isValidProfile(mainCPU) && isValidProfile(currentCPU) {
			report.WriteString("\n[diff] current vs main:\n")
			if out, err := output("go", "tool", "pprof", "-top", "-diff_base="+mainCPU, "-nodecount=20", currentCPU); err != nil {
				report.WriteString(fmt.Sprintf("  ERROR: %v\n", err))
			} else {
				report.WriteString(out + "\n")
			}
		}

		// Allocation hotspots.
		currentAllocs := filepath.Join(dir, tag, "current", "allocs.prof")
		if isValidProfile(currentAllocs) {
			report.WriteString("\n[current] Allocation Top 10:\n")
			if out, err := output("go", "tool", "pprof", "-top", "-alloc_space", "-nodecount=10", currentAllocs); err != nil {
				report.WriteString(fmt.Sprintf("  ERROR: %v\n", err))
			} else {
				report.WriteString(out + "\n")
			}
		}

		// perf stat summary.
		currentPerf := filepath.Join(dir, tag, "current", "perf-stat.txt")
		if data, err := os.ReadFile(currentPerf); err == nil && len(data) > 10 {
			report.WriteString("\n[current] perf stat:\n")
			report.WriteString(string(data) + "\n")
		}

		report.WriteString("\n\n")
	}

	// Try flamegraph SVGs (requires graphviz).
	svgGenerated := false
	for _, cfg := range profileConfigs {
		tag := fmt.Sprintf("%s-%s", cfg.engine, cfg.protocol)
		currentCPU := filepath.Join(dir, tag, "current", "cpu.prof")
		svgPath := filepath.Join(dir, tag, "current", "flamegraph.svg")
		if isValidProfile(currentCPU) {
			cmd := exec.Command("go", "tool", "pprof", "-svg", "-output="+svgPath, currentCPU)
			if err := cmd.Run(); err == nil {
				report.WriteString(fmt.Sprintf("Flamegraph: %s\n", svgPath))
				svgGenerated = true
			}
		}
	}
	if !svgGenerated {
		fmt.Println("Note: flamegraph SVGs not generated (graphviz may not be installed)")
	}

	analysisText := report.String()
	fmt.Print(analysisText)

	reportPath := filepath.Join(dir, "analysis.txt")
	if err := os.WriteFile(reportPath, []byte(analysisText), 0o644); err != nil {
		return err
	}
	fmt.Printf("\nAnalysis saved to: %s\n", reportPath)
	return nil
}

// isValidProfile checks if a profile file exists and is large enough to contain data.
func isValidProfile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 1000
}
