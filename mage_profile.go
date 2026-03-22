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

// profileConfigs defines the configs to profile (3 engines × 3 objectives × h2).
// H2 is chosen because it exercises more code paths (HPACK, flow control, streams).
var profileConfigs = []struct {
	engine    string
	objective string
	protocol  string
}{
	{"epoll", "latency", "h2"},
	{"epoll", "throughput", "h2"},
	{"epoll", "balanced", "h2"},
	{"iouring", "latency", "h2"},
	{"iouring", "throughput", "h2"},
	{"iouring", "balanced", "h2"},
	{"std", "latency", "h2"},
	{"std", "throughput", "h2"},
	{"std", "balanced", "h2"},
}

// LocalProfile captures pprof profiles (CPU, heap, allocs, goroutine) for
// both main and current branch across key engine configurations.
// On non-Linux hosts, a Multipass VM is created and destroyed.
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

	// Cross-compile profiled server + fullstack (load generator) for both branches.
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
	return runProfileOnTarget(dir, branch, func(version, engine, objective, protocol string) error {
		profiledBin := bins["profiled-main"]
		if version == "current" {
			profiledBin = bins["profiled-current"]
		}
		return profileOneConfig(dir, version, engine, objective, protocol,
			profiledBin, bins["fullstack-current"])
	})
}

func runProfileInVM(dir string, bins map[string]string, arch, branch string) error {
	fmt.Println("Not on Linux — running profiling in Multipass VM.")

	if err := ensureVMWithSpecs(profileVM, "4", "8G"); err != nil {
		return err
	}
	defer destroyVM(profileVM)

	// Install load generators.
	fmt.Println("Installing load tools (wrk, h2load)...")
	_, _ = vmExec(profileVM, "sudo apt-get update -qq && sudo apt-get install -y -qq wrk nghttp2-client curl")

	// Transfer binaries to VM.
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

	return runProfileOnTarget(dir, branch, func(version, engine, objective, protocol string) error {
		profiledBin := remoteBins["profiled-main"]
		if version == "current" {
			profiledBin = remoteBins["profiled-current"]
		}
		loadBin := remoteBins["fullstack-current"]
		tag := fmt.Sprintf("%s-%s-%s", engine, objective, protocol)

		// Create remote output dir.
		remoteDir := fmt.Sprintf("/tmp/profile/results/%s/%s", tag, version)
		_, _ = vmExec(profileVM, "mkdir -p "+remoteDir)

		// Build the profile capture script.
		script := buildProfileScript(profiledBin, loadBin, engine, objective, protocol, remoteDir)
		fmt.Printf("  Running profile script in VM...\n")
		if _, err := vmExec(profileVM, script); err != nil {
			fmt.Printf("  WARNING: profile capture failed: %v\n", err)
		}

		// Download profiles to local results dir.
		localDir := filepath.Join(dir, tag, version)
		_ = os.MkdirAll(localDir, 0o755)
		for _, f := range []string{"cpu.prof", "heap.prof", "allocs.prof", "goroutine.txt"} {
			remotePath := filepath.Join(remoteDir, f)
			localPath := filepath.Join(localDir, f)
			_ = transferFromVM(profileVM, remotePath, localPath)
		}
		return nil
	})
}

// runProfileOnTarget iterates through configs and branches, calling profileFn for each.
func runProfileOnTarget(dir, branch string, profileFn func(version, engine, objective, protocol string) error) error {
	for _, cfg := range profileConfigs {
		tag := fmt.Sprintf("%s-%s-%s", cfg.engine, cfg.objective, cfg.protocol)
		for _, version := range []string{"main", "current"} {
			label := "main"
			if version == "current" {
				label = branch
			}
			fmt.Printf("\n--- Profiling: %s %s ---\n", label, tag)
			if err := profileFn(version, cfg.engine, cfg.objective, cfg.protocol); err != nil {
				fmt.Printf("  WARNING: %s %s failed: %v\n", version, tag, err)
			}
		}
	}

	fmt.Printf("\n=== Profiles saved to %s ===\n", dir)
	fmt.Println("Analyze with: go tool pprof -http=:8080 <path-to-cpu.prof>")
	return nil
}

// profileOneConfig runs one profile capture (Linux direct mode).
func profileOneConfig(dir, version, engine, objective, protocol, profiledBin, loadBin string) error {
	tag := fmt.Sprintf("%s-%s-%s", engine, objective, protocol)
	localDir := filepath.Join(dir, tag, version)
	_ = os.MkdirAll(localDir, 0o755)

	script := buildProfileScript(profiledBin, loadBin, engine, objective, protocol, localDir)
	cmd := exec.Command("bash", "-c", script)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// buildProfileScript generates a bash script that starts the profiled server,
// applies sustained load via h2load/wrk, captures pprof profiles, and cleans up.
// The fullstack binary is self-contained and can't be used as an external load
// generator — we use h2load (H2) or wrk (H1) instead.
func buildProfileScript(profiledBin, _ /* loadBin unused */, engine, objective, protocol, outputDir string) string {
	// Choose load command based on protocol.
	var loadCmd string
	if protocol == "h2" {
		loadCmd = "h2load -c 64 -m 100 -t 4 -D 25 http://127.0.0.1:18080/ > /dev/null 2>&1 &"
	} else {
		loadCmd = "wrk -t4 -c64 -d25s http://127.0.0.1:18080/ > /dev/null 2>&1 &"
	}

	return strings.Join([]string{
		"set -e",
		fmt.Sprintf("ENGINE=%s OBJECTIVE=%s PROTOCOL=%s PORT=18080 %s &",
			engine, objective, protocol, profiledBin),
		"SERVER_PID=$!",
		"sleep 2",
		"",
		"# Sustained load (25s)",
		loadCmd,
		"LOAD_PID=$!",
		"",
		"# Wait for warmup, then capture profiles",
		"sleep 3",
		fmt.Sprintf("curl -s 'http://127.0.0.1:6060/debug/pprof/profile?seconds=18' -o %s/cpu.prof || true",
			outputDir),
		fmt.Sprintf("curl -s 'http://127.0.0.1:6060/debug/pprof/heap' -o %s/heap.prof || true",
			outputDir),
		fmt.Sprintf("curl -s 'http://127.0.0.1:6060/debug/pprof/allocs' -o %s/allocs.prof || true",
			outputDir),
		fmt.Sprintf("curl -s 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1' -o %s/goroutine.txt || true",
			outputDir),
		"",
		"# Cleanup",
		"kill $LOAD_PID 2>/dev/null || true",
		"kill $SERVER_PID 2>/dev/null || true",
		"wait $LOAD_PID 2>/dev/null || true",
		"wait $SERVER_PID 2>/dev/null || true",
		"sleep 1",
	}, "\n")
}

// CloudProfile captures pprof profiles on AWS EC2 instances.
// Same as LocalProfile but on cloud instances for production-representative results.
// Set CLOUD_ARCH=amd64|arm64 (default: amd64).
func CloudProfile() error {
	if err := awsEnsureCLI(); err != nil {
		return err
	}

	branch, err := currentBranch()
	if err != nil {
		return err
	}
	dir, err := resultsDir("cloud-profile")
	if err != nil {
		return err
	}

	arches := cloudArch()
	if len(arches) > 1 {
		// Default to single arch for profiling (it's expensive).
		arches = []string{arches[0]}
		fmt.Println("Note: CloudProfile runs one architecture at a time. Set CLOUD_ARCH to override.")
	}
	arch := arches[0]

	fmt.Printf("Cloud Profiling: %s vs main (linux/%s)\n\n", branch, arch)

	// Cross-compile binaries.
	bins := map[string]string{
		"profiled-current":  filepath.Join(dir, "profiled-current"),
		"profiled-main":     filepath.Join(dir, "profiled-main"),
		"fullstack-current": filepath.Join(dir, "fullstack-current"),
	}

	fmt.Println("Building binaries...")
	if err := crossCompile("test/benchmark/profiled", bins["profiled-current"], arch); err != nil {
		return err
	}
	if err := crossCompileFromRef("main", "test/benchmark/profiled", bins["profiled-main"], arch); err != nil {
		return err
	}
	if err := crossCompile("test/loadtest/fullstack", bins["fullstack-current"], arch); err != nil {
		return err
	}

	// Launch AWS instance.
	keyName, keyPath, err := awsCreateKeyPair(dir)
	if err != nil {
		return err
	}
	sgID, err := awsCreateSecurityGroup()
	if err != nil {
		awsDeleteKeyPair(keyName)
		return err
	}

	ami, err := awsLatestAMI(arch)
	if err != nil {
		return err
	}
	instanceID, ip, err := awsLaunchInstance(ami, awsInstanceType(arch), keyName, sgID, arch)
	if err != nil {
		return err
	}
	defer awsCleanup([]string{instanceID}, keyName, sgID, keyPath)

	if err := awsWaitSSH(ip, keyPath); err != nil {
		return err
	}

	// Transfer binaries.
	fmt.Println("Transferring binaries...")
	if _, err := awsSSH(ip, keyPath, "mkdir -p /tmp/profile/results"); err != nil {
		return err
	}
	for name, localPath := range bins {
		remotePath := "/tmp/profile/" + name
		if err := awsSCP(localPath, remotePath, ip, keyPath); err != nil {
			return fmt.Errorf("transfer %s: %w", name, err)
		}
	}
	if _, err := awsSSH(ip, keyPath, "chmod +x /tmp/profile/profiled-* /tmp/profile/fullstack-*"); err != nil {
		return err
	}

	// Install load generators and curl.
	fmt.Println("Installing load tools (wrk, h2load, curl)...")
	if _, err := awsSSH(ip, keyPath, "sudo apt-get update -qq && sudo apt-get install -y -qq wrk nghttp2-client curl"); err != nil {
		return fmt.Errorf("install tools: %w", err)
	}

	// Run profiles.
	return runProfileOnTarget(dir, branch, func(version, engine, objective, protocol string) error {
		profiledBin := "/tmp/profile/profiled-main"
		if version == "current" {
			profiledBin = "/tmp/profile/profiled-current"
		}
		loadBin := "/tmp/profile/fullstack-current"
		tag := fmt.Sprintf("%s-%s-%s", engine, objective, protocol)

		remoteDir := fmt.Sprintf("/tmp/profile/results/%s/%s", tag, version)
		_, _ = awsSSH(ip, keyPath, fmt.Sprintf("mkdir -p %s", remoteDir))

		script := buildProfileScript(profiledBin, loadBin, engine, objective, protocol, remoteDir)
		fmt.Printf("  Running profile capture on %s...\n", ip)
		if _, err := awsSSH(ip, keyPath, script); err != nil {
			fmt.Printf("  WARNING: profile capture failed: %v\n", err)
		}

		// Download profiles.
		localDir := filepath.Join(dir, tag, version)
		_ = os.MkdirAll(localDir, 0o755)
		for _, f := range []string{"cpu.prof", "heap.prof", "allocs.prof", "goroutine.txt"} {
			remotePath := filepath.Join(remoteDir, f)
			localPath := filepath.Join(localDir, f)
			_ = awsSCPDown(remotePath, localPath, ip, keyPath)
		}
		return nil
	})
}
