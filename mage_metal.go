//go:build mage

package main

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// benchRepoRelPath is the relative path to the benchmarks repo (sibling of celeris).
const benchRepoRelPath = "../benchmarks"

type metalConfig struct {
	engine, protocol string
}

func (c metalConfig) name() string {
	return fmt.Sprintf("%s-%s", c.engine, c.protocol)
}

func (c metalConfig) envVars(port string) string {
	return fmt.Sprintf("ENGINE=%s PROTOCOL=%s PORT=%s",
		c.engine, c.protocol, port)
}

// buildMetalConfigs generates all engine×protocol combinations.
func buildMetalConfigs() []metalConfig {
	var configs []metalConfig
	for _, eng := range []string{"iouring", "epoll", "adaptive"} {
		for _, proto := range []string{"h1", "h2", "hybrid"} {
			configs = append(configs, metalConfig{
				engine: eng, protocol: proto,
			})
		}
	}
	return configs
}

// CloudMetalBenchmark runs the full celeris benchmark matrix using wrk (H1) and
// h2load (H2) on separate EC2 instances. Tests 9 configurations (3 engines ×
// 3 protocols) across up to 3 celeris versions.
//
// Results are collected inline (like CloudBenchmarkSplit), aggregated into a
// comparison report, and saved to results/<timestamp>-cloud-metal-benchmark/.
//
// Server binaries are built from test/benchmark/server/ which accepts
// WORKERS env var for worker count control.
//
// Set CLOUD_ARCH=amd64|arm64 (default: arm64).
// Set METAL_INSTANCE=<type> (default: same as cloud benchmark instance).
// Set METAL_DURATION=15s (default) per benchmark.
// Set METAL_REFS=v1.0.0,HEAD,current (default) — refs to compare.
func CloudMetalBenchmark() error {
	if err := awsEnsureCLI(); err != nil {
		return err
	}

	branch, err := currentBranch()
	if err != nil {
		return err
	}
	dir, err := resultsDir("cloud-metal-benchmark")
	if err != nil {
		return err
	}

	arches := cloudArch()
	if len(arches) > 1 {
		arches = []string{arches[0]}
		fmt.Println("Note: CloudMetalBenchmark tests one architecture. Set CLOUD_ARCH to override.")
	}
	arch := arches[0]

	instanceType := metalEnvOr("METAL_INSTANCE", awsInstanceType(arch))
	duration := metalEnvOr("METAL_DURATION", "15s")
	refsStr := metalEnvOr("METAL_REFS", "v1.0.0,HEAD,current")
	refs := strings.Split(refsStr, ",")

	configs := buildMetalConfigs()

	fmt.Printf("Cloud Metal Benchmark: %s (%s on %s)\n", branch, arch, instanceType)
	fmt.Printf("Refs: %v\n", refs)
	fmt.Printf("Configs: %d (3 engines × 3 protocols)\n", len(configs))
	fmt.Printf("Duration: %s per benchmark, %d total runs\n\n", duration, len(configs)*len(refs))

	// Build server binaries for each ref.
	serverBins := make(map[string]string)
	for _, ref := range refs {
		label := sanitizeRef(ref)
		binPath := filepath.Join(dir, fmt.Sprintf("server-%s-%s", label, arch))
		fmt.Printf("Building server (%s)...\n", ref)
		if err := buildMetalServer(ref, binPath, arch); err != nil {
			return fmt.Errorf("build server %s: %w", ref, err)
		}
		serverBins[ref] = binPath
	}

	// AWS setup.
	keyName, keyPath, err := awsCreateKeyPair(dir)
	if err != nil {
		return err
	}
	sgID, err := awsCreateSecurityGroup()
	if err != nil {
		awsDeleteKeyPair(keyName)
		return err
	}
	_, _ = awsCLI("ec2", "authorize-security-group-ingress",
		"--region", awsRegion, "--group-id", sgID,
		"--protocol", "tcp", "--port", "18080", "--source-group", sgID)

	var allInstanceIDs []string
	defer func() {
		awsCleanup(allInstanceIDs, keyName, sgID, keyPath)
	}()

	ami, err := awsLatestAMI(arch)
	if err != nil {
		return err
	}

	// Launch server.
	serverID, serverPublicIP, err := awsLaunchInstance(ami, instanceType, keyName, sgID, arch)
	if err != nil {
		return err
	}
	allInstanceIDs = append(allInstanceIDs, serverID)
	if err := awsWaitSSH(serverPublicIP, keyPath); err != nil {
		return err
	}

	serverPrivateIP, err := awsCLI("ec2", "describe-instances",
		"--region", awsRegion, "--instance-ids", serverID,
		"--query", "Reservations[0].Instances[0].PrivateIpAddress", "--output", "text")
	if err != nil {
		return err
	}
	serverPrivateIP = strings.TrimSpace(serverPrivateIP)
	fmt.Printf("  Server: %s (private: %s)\n", serverPublicIP, serverPrivateIP)

	_, _ = awsSSH(serverPublicIP, keyPath, "echo 0 | sudo tee /proc/sys/kernel/apparmor_restrict_unprivileged_io_uring > /dev/null 2>&1")
	_, _ = awsSSH(serverPublicIP, keyPath, "mkdir -p /tmp/metal")

	for ref, bin := range serverBins {
		label := sanitizeRef(ref)
		if err := awsSCP(bin, fmt.Sprintf("/tmp/metal/server-%s", label), serverPublicIP, keyPath); err != nil {
			return fmt.Errorf("upload server %s: %w", ref, err)
		}
	}
	_, _ = awsSSH(serverPublicIP, keyPath, "chmod +x /tmp/metal/server-*")

	// Launch client.
	clientID, clientPublicIP, err := awsLaunchInstance(ami, instanceType, keyName, sgID, arch)
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
	// Ensure /tmp/metal exists on client before any file transfers.
	if _, err := awsSSH(clientPublicIP, keyPath, "mkdir -p /tmp/metal"); err != nil {
		return fmt.Errorf("mkdir on client: %w", err)
	}
	if err := awsSCP(keyPath, "/tmp/server-key.pem", clientPublicIP, keyPath); err != nil {
		return fmt.Errorf("upload SSH key to client: %w", err)
	}
	if _, err := awsSSH(clientPublicIP, keyPath, "chmod 600 /tmp/server-key.pem"); err != nil {
		return err
	}

	sshToServer := fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o BatchMode=yes -i /tmp/server-key.pem ubuntu@%s", serverPrivateIP)

	// Collect results: map[ref]map[configName]rps
	results := make(map[string]map[string]float64)
	for _, ref := range refs {
		results[ref] = make(map[string]float64)
	}
	var failures []string

	// Run benchmarks — interleaved per config for fairness.
	run := 0
	for i, cfg := range configs {
		fmt.Printf("\n--- Config %d/%d: %s ---\n", i+1, len(configs), cfg.name())

		for _, ref := range refs {
			run++
			label := sanitizeRef(ref)
			serverBinRemote := fmt.Sprintf("/tmp/metal/server-%s", label)

			// Start server.
			startScript := strings.Join([]string{
				fmt.Sprintf("%s 'sudo pkill -9 -f server- 2>/dev/null || true'", sshToServer),
				"sleep 1",
				fmt.Sprintf("%s 'sudo prlimit --memlock=unlimited env %s %s > /tmp/metal/server.log 2>&1 &'",
					sshToServer, cfg.envVars("18080"), serverBinRemote),
				"sleep 3",
				fmt.Sprintf("if ! %s 'timeout 2 bash -c \"echo > /dev/tcp/localhost/18080\" 2>/dev/null'; then", sshToServer),
				"  echo 'ERROR: server not listening'",
				fmt.Sprintf("  %s 'cat /tmp/metal/server.log' || true", sshToServer),
				"fi",
			}, "\n")

			scriptPath := filepath.Join(dir, fmt.Sprintf("start-%d.sh", run))
			_ = os.WriteFile(scriptPath, []byte(startScript), 0o755)
			if err := awsSCP(scriptPath, "/tmp/metal/start.sh", clientPublicIP, keyPath); err != nil {
				continue
			}
			startOut, err := awsSSH(clientPublicIP, keyPath, "bash /tmp/metal/start.sh")
			if err != nil || strings.Contains(startOut, "ERROR") {
				fmt.Printf("  %s: FAILED (server start)\n", label)
				failures = append(failures, fmt.Sprintf("%s/%s: server start failed", cfg.name(), ref))
				continue
			}

			// Run load.
			var loadCmd string
			if cfg.protocol == "h2" {
				loadCmd = fmt.Sprintf("h2load -c128 -m128 -t4 -D %s http://%s:18080/", duration, serverPrivateIP)
			} else {
				loadCmd = fmt.Sprintf("wrk -t4 -c256 -d%s --latency http://%s:18080/", duration, serverPrivateIP)
			}

			loadOut, _ := awsSSH(clientPublicIP, keyPath, loadCmd)

			// Parse RPS.
			var rps float64
			if cfg.protocol == "h2" {
				if m := h2loadRPSRegex.FindStringSubmatch(loadOut); m != nil {
					rps, _ = strconv.ParseFloat(m[1], 64)
				}
			} else {
				if m := wrkRPSRegex.FindStringSubmatch(loadOut); m != nil {
					rps, _ = strconv.ParseFloat(m[1], 64)
				}
			}

			results[ref][cfg.name()] = rps
			fmt.Printf("  %s: %.0f rps\n", label, rps)

			// Stop server.
			_, _ = awsSSH(clientPublicIP, keyPath, fmt.Sprintf("%s 'sudo pkill -9 -f server- 2>/dev/null || true'", sshToServer))
			time.Sleep(2 * time.Second)
		}
	}

	// Generate report.
	report := generateMetalReport(branch, arch, instanceType, refs, configs, results, failures)
	fmt.Println("\n" + report)

	reportPath := filepath.Join(dir, fmt.Sprintf("report-%s.txt", arch))
	_ = os.WriteFile(reportPath, []byte(report), 0o644)
	fmt.Printf("Report saved to: %s\n", reportPath)

	return nil
}

// generateMetalReport builds the comparison report from collected results.
func generateMetalReport(branch, arch, instance string, refs []string, configs []metalConfig, results map[string]map[string]float64, failures []string) string {
	var sb strings.Builder

	sb.WriteString("Celeris Metal Benchmark Report\n")
	sb.WriteString(fmt.Sprintf("Branch: %s (%s on %s)\n", branch, arch, instance))
	sb.WriteString(fmt.Sprintf("Date: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("Refs: %s\n", strings.Join(refs, ", ")))
	sb.WriteString(fmt.Sprintf("Configs: %d\n\n", len(configs)))

	// Header row.
	header := fmt.Sprintf("%-40s", "Config")
	for _, ref := range refs {
		header += fmt.Sprintf(" | %14s", ref)
	}
	if len(refs) >= 2 {
		header += fmt.Sprintf(" | %8s", "delta")
	}
	sb.WriteString(header + "\n")
	sb.WriteString(strings.Repeat("-", len(header)+5) + "\n")

	// Sort configs by name for consistent output.
	sorted := make([]metalConfig, len(configs))
	copy(sorted, configs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].name() < sorted[j].name() })

	improved, neutral, regressed := 0, 0, 0
	var totalDelta float64
	counted := 0

	for _, cfg := range sorted {
		row := fmt.Sprintf("%-40s", cfg.name())
		rpsList := make([]float64, len(refs))
		for i, ref := range refs {
			rps := results[ref][cfg.name()]
			rpsList[i] = rps
			if rps > 0 {
				row += fmt.Sprintf(" | %14.0f", rps)
			} else {
				row += fmt.Sprintf(" | %14s", "FAIL")
			}
		}

		if len(refs) >= 2 {
			first := rpsList[0]
			last := rpsList[len(rpsList)-1]
			if first > 0 && last > 0 {
				delta := (last - first) / first * 100
				row += fmt.Sprintf(" | %+7.1f%%", delta)
				totalDelta += delta
				counted++
				if math.Abs(delta) < 2.0 {
					neutral++
				} else if delta > 0 {
					improved++
				} else {
					regressed++
				}
			} else {
				row += fmt.Sprintf(" | %8s", "N/A")
			}
		}
		sb.WriteString(row + "\n")
	}

	sb.WriteString(strings.Repeat("-", 100) + "\n")
	avgDelta := 0.0
	if counted > 0 {
		avgDelta = totalDelta / float64(counted)
	}
	sb.WriteString(fmt.Sprintf("\nSummary (%s vs %s): %d improved, %d neutral, %d regressed (avg: %+.1f%%)\n",
		refs[0], refs[len(refs)-1], improved, neutral, regressed, avgDelta))

	// Per-engine summary.
	sb.WriteString("\n--- Per-engine averages ---\n")
	for _, eng := range []string{"iouring", "epoll", "adaptive"} {
		var sum float64
		var n int
		for _, cfg := range sorted {
			if cfg.engine != eng {
				continue
			}
			if len(refs) < 2 {
				continue
			}
			first := results[refs[0]][cfg.name()]
			last := results[refs[len(refs)-1]][cfg.name()]
			if first > 0 && last > 0 {
				sum += (last - first) / first * 100
				n++
			}
		}
		if n > 0 {
			sb.WriteString(fmt.Sprintf("  %-12s: %+.1f%% (%d configs)\n", eng, sum/float64(n), n))
		}
	}

	// Per-protocol summary.
	sb.WriteString("\n--- Per-protocol averages ---\n")
	for _, proto := range []string{"h1", "h2", "hybrid"} {
		var sum float64
		var n int
		for _, cfg := range sorted {
			if cfg.protocol != proto {
				continue
			}
			if len(refs) < 2 {
				continue
			}
			first := results[refs[0]][cfg.name()]
			last := results[refs[len(refs)-1]][cfg.name()]
			if first > 0 && last > 0 {
				sum += (last - first) / first * 100
				n++
			}
		}
		if n > 0 {
			sb.WriteString(fmt.Sprintf("  %-12s: %+.1f%% (%d configs)\n", proto, sum/float64(n), n))
		}
	}

	// Failures.
	if len(failures) > 0 {
		sb.WriteString(fmt.Sprintf("\n--- Failures (%d) ---\n", len(failures)))
		for _, f := range failures {
			sb.WriteString(fmt.Sprintf("  %s\n", f))
		}
	}

	return sb.String()
}

// buildMetalServer builds test/benchmark/server linked against a specific
// celeris version. For "current", builds from the working tree. For published
// tags (v1.0.0 etc.), creates a temp module that imports the server code from
// HEAD but replaces the celeris dependency with the tagged version from the
// Go module proxy. This works even for old tags that don't have the server binary.
func buildMetalServer(ref, outputPath, arch string) error {
	if ref == "current" {
		return crossCompile("test/benchmark/server", outputPath, arch)
	}
	if ref == "HEAD" {
		return crossCompileFromRef("HEAD", "test/benchmark/server", outputPath, arch)
	}

	// For tagged versions: build from HEAD's server code but replace celeris
	// module with the published version. This ensures the server binary always
	// uses the latest routes/control logic but links against the specified
	// celeris engine code.
	absOutput, err := filepath.Abs(outputPath)
	if err != nil {
		return err
	}
	absCelerisDir, err := filepath.Abs(".")
	if err != nil {
		return err
	}

	// Create a worktree from HEAD (which has test/benchmark/server/).
	worktreeDir := filepath.Join(os.TempDir(), fmt.Sprintf("celeris-metal-%s-%d", ref, time.Now().UnixNano()))
	fmt.Printf("  Creating worktree from HEAD for server build with celeris@%s\n", ref)
	if err := run("git", "worktree", "add", worktreeDir, "HEAD"); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	defer func() {
		_ = exec.Command("git", "worktree", "remove", "--force", worktreeDir).Run()
	}()

	// Replace celeris module with the published tag version.
	// First drop the local replace (if any), then require the tag.
	goModEdit := exec.Command("go", "mod", "edit",
		fmt.Sprintf("-require=github.com/goceleris/celeris@%s", ref),
		"-dropreplace=github.com/goceleris/celeris")
	goModEdit.Dir = worktreeDir
	goModEdit.Stdout = os.Stdout
	goModEdit.Stderr = os.Stderr
	if err := goModEdit.Run(); err != nil {
		// If require fails (self-referencing module), use replace instead.
		// For a module that references itself, we need a different approach:
		// point at the tag via a local checkout.
		tagWorktree := filepath.Join(os.TempDir(), fmt.Sprintf("celeris-tag-%s-%d", ref, time.Now().UnixNano()))
		if err := run("git", "worktree", "add", tagWorktree, ref); err != nil {
			return fmt.Errorf("git worktree add %s: %w", ref, err)
		}
		defer func() {
			_ = exec.Command("git", "worktree", "remove", "--force", tagWorktree).Run()
		}()

		replaceCmd := exec.Command("go", "mod", "edit",
			fmt.Sprintf("-replace=github.com/goceleris/celeris=%s", tagWorktree))
		replaceCmd.Dir = worktreeDir
		replaceCmd.Stdout = os.Stdout
		replaceCmd.Stderr = os.Stderr
		if err := replaceCmd.Run(); err != nil {
			return fmt.Errorf("go mod edit -replace: %w", err)
		}
	}

	_ = absCelerisDir // suppress unused warning

	tidyCmd := exec.Command("go", "mod", "tidy")
	tidyCmd.Dir = worktreeDir
	tidyCmd.Stdout = os.Stdout
	tidyCmd.Stderr = os.Stderr
	_ = tidyCmd.Run() // best effort

	buildCmd := exec.Command("go", "build", "-o", absOutput, "./test/benchmark/server")
	buildCmd.Dir = worktreeDir
	buildCmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	return buildCmd.Run()
}

// metalCrossCompile builds a package from an external directory.
func metalCrossCompile(repoDir, pkg, outputPath, arch string) error {
	absOutput, err := filepath.Abs(outputPath)
	if err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-o", absOutput, "./"+pkg)
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func sanitizeRef(ref string) string {
	return strings.NewReplacer("/", "-", ".", "-").Replace(ref)
}

func metalEnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
