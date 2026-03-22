//go:build mage

package main

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// hostArch returns "amd64" or "arm64" matching the host CPU.
func hostArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}

// resultsDir creates and returns a timestamped results directory.
func resultsDir(label string) (string, error) {
	ts := time.Now().Format("20060102-150405")
	dir := filepath.Join("results", ts+"-"+label)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create results dir: %w", err)
	}
	return dir, nil
}

// currentBranch returns the current git branch name.
func currentBranch() (string, error) {
	return output("git", "rev-parse", "--abbrev-ref", "HEAD")
}

// crossCompile builds a Go package for linux/<arch> from the current tree.
func crossCompile(pkg, outputPath, arch string) error {
	fmt.Printf("  Cross-compiling %s for linux/%s → %s\n", pkg, arch, outputPath)
	return runEnv(map[string]string{
		"GOOS":        "linux",
		"GOARCH":      arch,
		"CGO_ENABLED": "0",
	}, "go", "build", "-o", outputPath, "./"+pkg)
}

// crossCompileFromRef creates a git worktree for the given ref, builds the
// package from it, then removes the worktree. The built binary uses the ref's
// own go.mod so dependency differences don't cause issues.
func crossCompileFromRef(ref, pkg, outputPath, arch string) error {
	worktreeDir := filepath.Join(os.TempDir(), fmt.Sprintf("celeris-worktree-%s-%d", ref, time.Now().UnixNano()))
	fmt.Printf("  Cross-compiling %s@%s for linux/%s\n", pkg, ref, arch)

	if err := run("git", "worktree", "add", worktreeDir, ref); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	defer func() {
		_ = exec.Command("git", "worktree", "remove", "--force", worktreeDir).Run()
	}()

	absOutput, err := filepath.Abs(outputPath)
	if err != nil {
		return err
	}

	cmd := exec.Command("go", "build", "-o", absOutput, "./"+pkg)
	cmd.Dir = worktreeDir
	cmd.Env = append(os.Environ(),
		"GOOS=linux",
		"GOARCH="+arch,
		"CGO_ENABLED=0",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ConfigResult holds parsed fullstack-loadtest output for one config.
type ConfigResult struct {
	Name     string
	Requests int64
	RPS      float64
	Errors   int64
	Status   string
}

// rpsRegex matches fullstack-loadtest output lines like:
// [PASS] celeris-iouring-latency-h1: 451340 reqs, 0 errs, 5s — H1: 451340 reqs, 90261 rps
var rpsRegex = regexp.MustCompile(`\[(PASS|FAIL)\]\s+(\S+):\s+(\d+)\s+reqs,\s+(\d+)\s+errs,.*?(\d+)\s+rps`)

// parseFullstackOutput parses fullstack-loadtest stdout into structured results.
func parseFullstackOutput(out string) []ConfigResult {
	var results []ConfigResult
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		// Strip ANSI escape codes.
		line = regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(line, "")
		m := rpsRegex.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		reqs, _ := strconv.ParseInt(m[3], 10, 64)
		errs, _ := strconv.ParseInt(m[4], 10, 64)
		rps, _ := strconv.ParseFloat(m[5], 64)
		results = append(results, ConfigResult{
			Name:     m[2],
			Requests: reqs,
			Errors:   errs,
			RPS:      rps,
			Status:   m[1],
		})
	}
	return results
}

// medianRPS computes the median RPS for each config across multiple runs.
func medianRPS(runs [][]ConfigResult) map[string]float64 {
	byConfig := make(map[string][]float64)
	for _, run := range runs {
		for _, r := range run {
			byConfig[r.Name] = append(byConfig[r.Name], r.RPS)
		}
	}
	medians := make(map[string]float64)
	for name, vals := range byConfig {
		sort.Float64s(vals)
		n := len(vals)
		if n == 0 {
			continue
		}
		if n%2 == 0 {
			medians[name] = (vals[n/2-1] + vals[n/2]) / 2
		} else {
			medians[name] = vals[n/2]
		}
	}
	return medians
}

// generateABReport produces a comparison table from interleaved A/B results.
func generateABReport(branchName string, mainRuns, branchRuns [][]ConfigResult) string {
	mainMedians := medianRPS(mainRuns)
	branchMedians := medianRPS(branchRuns)

	// Collect all config names in order.
	var configs []string
	seen := make(map[string]bool)
	for _, runs := range append(mainRuns, branchRuns...) {
		for _, r := range runs {
			if !seen[r.Name] {
				seen[r.Name] = true
				configs = append(configs, r.Name)
			}
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Celeris A/B Benchmark Report\n"))
	sb.WriteString(fmt.Sprintf("Branch: %s vs main\n", branchName))
	sb.WriteString(fmt.Sprintf("Date: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("Rounds: %d per branch (interleaved)\n\n", len(mainRuns)))

	sb.WriteString(fmt.Sprintf("%-40s | %12s | %12s | %8s\n", "Config", "main (med)", "branch (med)", "delta"))
	sb.WriteString(strings.Repeat("-", 80) + "\n")

	improved, neutral, regressed := 0, 0, 0
	var totalDelta float64

	for _, name := range configs {
		mRPS := mainMedians[name]
		bRPS := branchMedians[name]
		delta := 0.0
		deltaStr := "N/A"
		if mRPS > 0 {
			delta = (bRPS - mRPS) / mRPS * 100
			deltaStr = fmt.Sprintf("%+.1f%%", delta)
		}
		sb.WriteString(fmt.Sprintf("%-40s | %12.0f | %12.0f | %8s\n", name, mRPS, bRPS, deltaStr))

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

	return sb.String()
}

// destroyVM stops and deletes a Multipass VM, ignoring errors.
func destroyVM(name string) {
	fmt.Printf("Cleaning up VM %q...\n", name)
	_ = exec.Command("multipass", "stop", name).Run()
	_ = exec.Command("multipass", "delete", name).Run()
	_ = exec.Command("multipass", "purge").Run()
}

// ensureVMWithSpecs creates a Multipass VM with the given specs if it doesn't exist.
func ensureVMWithSpecs(name, cpus, memory string) error {
	goVer, err := goVersion()
	if err != nil {
		return err
	}
	arch := vmArch()

	if !multipassVMExists(name) {
		fmt.Printf("Creating Multipass VM %q (Go %s, linux/%s, %s CPUs, %s RAM)...\n",
			name, goVer, arch, cpus, memory)
		if err := run("multipass", "launch", "--name", name,
			"--cpus", cpus, "--memory", memory, "--disk", "20G", "noble"); err != nil {
			return fmt.Errorf("failed to create VM: %w", err)
		}
		fmt.Println("Installing Go from go.dev...")
		script := installGoScript(goVer, arch)
		if err := run("multipass", "exec", name, "--", "bash", "-c", script); err != nil {
			return fmt.Errorf("Go install failed: %w", err)
		}
	}
	return ensureVMRunning(name)
}

// syncSourceTo syncs the current source tree to a named VM.
func syncSourceTo(name string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	fmt.Printf("Syncing source to VM %q...\n", name)
	_ = run("multipass", "umount", name+":celeris")
	_ = run("multipass", "exec", name, "--", "rm", "-rf", vmProjectDir)
	if err := run("multipass", "transfer", "-r", cwd, name+":"+vmProjectDir); err != nil {
		return fmt.Errorf("failed to sync source: %w", err)
	}
	return nil
}

// vmExec runs a command in a Multipass VM and returns stdout+stderr.
func vmExec(name, script string) (string, error) {
	cmd := exec.Command("multipass", "exec", name, "--", "bash", "-c", script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// vmExecStream runs a command in a Multipass VM with stdout/stderr streaming.
func vmExecStream(name, script string) error {
	return run("multipass", "exec", name, "--", "bash", "-c", script)
}

// transferToVM copies a local file into the VM at the given path.
func transferToVM(name, localPath, remotePath string) error {
	// Ensure parent directory exists.
	remoteDir := filepath.Dir(remotePath)
	_ = exec.Command("multipass", "exec", name, "--", "mkdir", "-p", remoteDir).Run()
	return run("multipass", "transfer", localPath, name+":"+remotePath)
}

// transferFromVM copies a file from the VM to a local path.
func transferFromVM(name, remotePath, localPath string) error {
	return run("multipass", "transfer", name+":"+remotePath, localPath)
}

// runInterleaved executes 6 interleaved fullstack runs (3 per branch).
// exec takes a binary path and returns its stdout.
func runInterleaved(
	execFn func(binary string) (string, error),
	mainBin, currentBin string,
) (mainRuns, currentRuns [][]ConfigResult, err error) {
	// Pattern: M1 → C1 → C2 → M2 → M3 → C3
	// Cancels thermal throttling and CPU frequency drift.
	type run struct {
		label  string
		binary string
		target *[][]ConfigResult
	}
	schedule := []run{
		{"main R1", mainBin, &mainRuns},
		{"branch R1", currentBin, &currentRuns},
		{"branch R2", currentBin, &currentRuns},
		{"main R2", mainBin, &mainRuns},
		{"main R3", mainBin, &mainRuns},
		{"branch R3", currentBin, &currentRuns},
	}

	for i, r := range schedule {
		fmt.Printf("\n--- Round %d/6: %s ---\n", i+1, r.label)
		out, execErr := execFn(r.binary)
		if execErr != nil {
			fmt.Printf("WARNING: %s failed: %v\n", r.label, execErr)
		}
		fmt.Print(out)
		results := parseFullstackOutput(out)
		*r.target = append(*r.target, results)
	}

	return mainRuns, currentRuns, nil
}
