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
// Both engines and both protocols for fast iteration cycles.
var profileConfigs = []struct {
	engine   string
	protocol string
}{
	{"epoll", "h1"},
	{"iouring", "h1"},
	{"epoll", "h2"},
	{"iouring", "h2"},
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
		arches = []string{arches[0]}
		fmt.Println("Note: CloudProfile runs one architecture at a time. Set CLOUD_ARCH to override.")
	}
	arch := arches[0]

	fmt.Printf("Cloud Profiling: %s vs main (linux/%s)\n\n", branch, arch)

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

	fmt.Println("Installing load tools (wrk, h2load, curl, perf)...")
	if _, err := awsSSH(ip, keyPath, "sudo dnf install -y -q wrk nghttp2 2>/dev/null || (sudo apt-get update -qq && sudo apt-get install -y -qq wrk nghttp2-client curl) || true"); err != nil {
		return fmt.Errorf("install tools: %w", err)
	}
	_, _ = awsSSH(ip, keyPath, "sudo apt-get install -y -qq linux-tools-$(uname -r) linux-tools-common 2>/dev/null || true")
	_, _ = awsSSH(ip, keyPath, "sudo sysctl -w kernel.io_uring_disabled=0 2>/dev/null; sudo sysctl -w kernel.io_uring_group=-1 2>/dev/null; echo 0 | sudo tee /proc/sys/kernel/apparmor_restrict_unprivileged_io_uring 2>/dev/null")

	return runProfileOnTarget(dir, branch, func(version, engine, protocol string) error {
		profiledBin := "/tmp/profile/profiled-main"
		if version == "current" {
			profiledBin = "/tmp/profile/profiled-current"
		}
		loadBin := "/tmp/profile/fullstack-current"
		tag := fmt.Sprintf("%s-%s", engine, protocol)

		remoteDir := fmt.Sprintf("/tmp/profile/results/%s/%s", tag, version)
		_, _ = awsSSH(ip, keyPath, fmt.Sprintf("mkdir -p %s", remoteDir))

		script := buildProfileScript(profiledBin, loadBin, engine, protocol, remoteDir)
		fmt.Printf("  Running profile capture on %s...\n", ip)
		if _, err := awsSSH(ip, keyPath, script); err != nil {
			fmt.Printf("  WARNING: profile capture failed: %v\n", err)
		}

		localDir := filepath.Join(dir, tag, version)
		_ = os.MkdirAll(localDir, 0o755)
		for _, f := range profileArtifacts {
			remotePath := filepath.Join(remoteDir, f)
			localPath := filepath.Join(localDir, f)
			_ = awsSCPDown(remotePath, localPath, ip, keyPath)
		}
		return nil
	})
}

// CloudProfileSplit captures pprof profiles using separate server and client
// EC2 instances. The server gets all 8 CPUs (no taskset), providing
// production-representative CPU profiles. Profile capture and perf stat run
// on the server machine itself.
//
// Set CLOUD_ARCH=amd64|arm64 (default: amd64).
func CloudProfileSplit() error {
	if err := awsEnsureCLI(); err != nil {
		return err
	}

	branch, err := currentBranch()
	if err != nil {
		return err
	}
	dir, err := resultsDir("cloud-profile-split")
	if err != nil {
		return err
	}

	arches := cloudArch()
	if len(arches) > 1 {
		arches = []string{arches[0]}
		fmt.Println("Note: CloudProfileSplit tests one architecture. Set CLOUD_ARCH to override.")
	}
	arch := arches[0]

	fmt.Printf("Cloud Split Profiling: %s vs main (linux/%s)\n", branch, arch)
	fmt.Printf("Separate server (all 8 CPUs) + client machines\n\n")

	bins := map[string]string{
		"profiled-current": filepath.Join(dir, "profiled-current"),
		"profiled-main":    filepath.Join(dir, "profiled-main"),
	}

	fmt.Println("Building binaries...")
	if err := crossCompile("test/benchmark/profiled", bins["profiled-current"], arch); err != nil {
		return err
	}
	if err := crossCompileFromRef("main", "test/benchmark/profiled", bins["profiled-main"], arch); err != nil {
		return err
	}

	keyName, keyPath, err := awsCreateKeyPair(dir)
	if err != nil {
		return err
	}
	sgID, err := awsCreateSecurityGroup()
	if err != nil {
		awsDeleteKeyPair(keyName)
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

	// Setup server: binaries, perf, io_uring.
	fmt.Println("Setting up server...")
	if _, err := awsSSH(serverPublicIP, keyPath, "mkdir -p /tmp/profile/results"); err != nil {
		return err
	}
	for name, localPath := range bins {
		remotePath := "/tmp/profile/" + name
		if err := awsSCP(localPath, remotePath, serverPublicIP, keyPath); err != nil {
			return fmt.Errorf("transfer %s: %w", name, err)
		}
	}
	if _, err := awsSSH(serverPublicIP, keyPath, "chmod +x /tmp/profile/profiled-*"); err != nil {
		return err
	}
	_, _ = awsSSH(serverPublicIP, keyPath, "sudo apt-get update -qq && sudo apt-get install -y -qq linux-tools-$(uname -r) linux-tools-common curl 2>/dev/null || true")
	_, _ = awsSSH(serverPublicIP, keyPath, "sudo sysctl -w kernel.io_uring_disabled=0 2>/dev/null; sudo sysctl -w kernel.io_uring_group=-1 2>/dev/null; echo 0 | sudo tee /proc/sys/kernel/apparmor_restrict_unprivileged_io_uring 2>/dev/null; echo setup done")

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

	// Profile each config and version.
	for _, cfg := range profileConfigs {
		tag := fmt.Sprintf("%s-%s", cfg.engine, cfg.protocol)
		for _, version := range []string{"main", "current"} {
			label := "main"
			if version == "current" {
				label = branch
			}
			fmt.Printf("\n--- Profiling: %s %s ---\n", label, tag)

			profiledBin := "/tmp/profile/profiled-main"
			if version == "current" {
				profiledBin = "/tmp/profile/profiled-current"
			}

			remoteDir := fmt.Sprintf("/tmp/profile/results/%s/%s", tag, version)
			_, _ = awsSSH(serverPublicIP, keyPath, "mkdir -p "+remoteDir)

			script := buildSplitProfilePassScript(profiledBin, cfg.engine, cfg.protocol,
				serverPrivateIP, "/tmp/server-key.pem", remoteDir)

			scriptPath := filepath.Join(dir, fmt.Sprintf("profile-%s-%s.sh", tag, version))
			_ = os.WriteFile(scriptPath, []byte(script), 0o755)
			if err := awsSCP(scriptPath, "/tmp/profile-run.sh", clientPublicIP, keyPath); err != nil {
				fmt.Printf("  WARNING: script upload failed: %v\n", err)
				continue
			}
			out, err := awsSSH(clientPublicIP, keyPath, "bash /tmp/profile-run.sh")
			if err != nil {
				fmt.Printf("  WARNING: profile capture failed: %v\nOutput: %s\n", err, out)
			} else {
				fmt.Print(out)
			}

			// Download profiles from server.
			localDir := filepath.Join(dir, tag, version)
			_ = os.MkdirAll(localDir, 0o755)
			for _, f := range profileArtifacts {
				remotePath := filepath.Join(remoteDir, f)
				localPath := filepath.Join(localDir, f)
				_ = awsSCPDown(remotePath, localPath, serverPublicIP, keyPath)
			}
		}
	}

	fmt.Printf("\n=== Profiles saved to %s ===\n", dir)

	if err := analyzeProfiles(dir); err != nil {
		fmt.Printf("WARNING: profile analysis failed: %v\n", err)
	}

	return nil
}

// buildSplitProfilePassScript generates a bash script that runs on the CLIENT
// machine for one profile capture. It starts the server via SSH, runs load
// locally, and triggers profile + perf stat capture on the server.
func buildSplitProfilePassScript(serverBin, engine, protocol, serverIP, serverKeyPath, remoteOutputDir string) string {
	sshBase := fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o BatchMode=yes -o ServerAliveInterval=30 -o ConnectTimeout=10 -i %s ubuntu@%s",
		serverKeyPath, serverIP)

	var loadCmd string
	if protocol == "h2" {
		loadCmd = fmt.Sprintf("h2load -c128 -m128 -t4 -D 25 http://%s:18080/", serverIP)
	} else {
		loadCmd = fmt.Sprintf("wrk -t4 -c256 -d25s http://%s:18080/", serverIP)
	}

	// Server startup script (uploaded and executed on server).
	serverStartScript := strings.Join([]string{
		"sudo pkill -9 -f profiled 2>/dev/null || true",
		"sleep 0.5",
		"echo 0 | sudo tee /proc/sys/kernel/apparmor_restrict_unprivileged_io_uring > /dev/null 2>&1 || true",
		fmt.Sprintf("sudo env ENGINE=%s PROTOCOL=%s PORT=18080 prlimit --memlock=unlimited %s > /tmp/profiled.log 2>&1 &",
			engine, protocol, serverBin),
		"sleep 3",
		"if timeout 2 bash -c 'echo > /dev/tcp/127.0.0.1/18080' 2>/dev/null; then echo SERVER_READY; else echo SERVER_FAILED; cat /tmp/profiled.log 2>/dev/null; fi",
		"if timeout 2 bash -c 'echo > /dev/tcp/127.0.0.1/6060' 2>/dev/null; then echo PPROF_READY; else echo PPROF_FAILED; fi",
	}, "\n")

	// Profile capture script (run on server while load is active).
	profileCaptureScript := strings.Join([]string{
		fmt.Sprintf("mkdir -p %s", remoteOutputDir),
		"echo 'Capturing CPU profile (20s)...'",
		fmt.Sprintf("HTTP_CODE=$(curl -s -w '%%{http_code}' -o %s/cpu.prof 'http://127.0.0.1:6060/debug/pprof/profile?seconds=20')", remoteOutputDir),
		"if [ \"$HTTP_CODE\" != \"200\" ]; then",
		"  echo \"CPU profile: HTTP $HTTP_CODE\"",
		"else",
		fmt.Sprintf("  SIZE=$(stat -c %%s %s/cpu.prof 2>/dev/null || echo 0)", remoteOutputDir),
		"  echo \"CPU profile: $SIZE bytes\"",
		"  if [ \"$SIZE\" -lt 1000 ]; then echo 'WARNING: CPU profile < 1KB — likely empty'; fi",
		"fi",
		fmt.Sprintf("curl -sf -o %s/heap.prof 'http://127.0.0.1:6060/debug/pprof/heap' || true", remoteOutputDir),
		fmt.Sprintf("curl -sf -o %s/allocs.prof 'http://127.0.0.1:6060/debug/pprof/allocs' || true", remoteOutputDir),
		fmt.Sprintf("curl -sf -o %s/goroutine.txt 'http://127.0.0.1:6060/debug/pprof/goroutine?debug=1' || true", remoteOutputDir),
		fmt.Sprintf("ls -la %s/ 2>/dev/null || true", remoteOutputDir),
	}, "\n")

	// perf stat script (run on server for 15s).
	perfStatScript := strings.Join([]string{
		"SERVER_PID=$(pgrep -f profiled | grep -v pgrep | head -1)",
		"if [ -n \"$SERVER_PID\" ] && command -v perf > /dev/null 2>&1; then",
		fmt.Sprintf("  sudo perf stat -e cycles,instructions,cache-misses,cache-references,context-switches,cpu-migrations,page-faults -p $SERVER_PID -- sleep 15 > %s/perf-stat.txt 2>&1", remoteOutputDir),
		"  echo 'perf stat complete'",
		"else",
		"  echo 'perf stat skipped (perf not installed or PID not found)'",
		"fi",
	}, "\n")

	return strings.Join([]string{
		"set -e",
		"",
		"# Upload server startup script.",
		"cat > /tmp/server-start.sh << 'SERVEREOF'",
		serverStartScript,
		"SERVEREOF",
		fmt.Sprintf("scp -o StrictHostKeyChecking=no -o BatchMode=yes -i %s /tmp/server-start.sh ubuntu@%s:/tmp/server-start.sh 2>/dev/null",
			serverKeyPath, serverIP),
		fmt.Sprintf("%s 'bash /tmp/server-start.sh'", sshBase),
		"",
		"# Verify server is reachable from client.",
		fmt.Sprintf("if ! timeout 10 bash -c 'until echo > /dev/tcp/%s/18080 2>/dev/null; do sleep 0.5; done'; then", serverIP),
		"  echo 'ERROR: server not reachable from client'",
		fmt.Sprintf("  %s 'sudo pkill -9 -f profiled 2>/dev/null; true'", sshBase),
		"  exit 1",
		"fi",
		"echo 'Server reachable.'",
		"",
		"# Upload profile capture and perf stat scripts.",
		"cat > /tmp/profile-capture.sh << 'PROFEOF'",
		profileCaptureScript,
		"PROFEOF",
		fmt.Sprintf("scp -o StrictHostKeyChecking=no -o BatchMode=yes -i %s /tmp/profile-capture.sh ubuntu@%s:/tmp/profile-capture.sh 2>/dev/null",
			serverKeyPath, serverIP),
		"",
		"cat > /tmp/perf-stat.sh << 'PERFEOF'",
		perfStatScript,
		"PERFEOF",
		fmt.Sprintf("scp -o StrictHostKeyChecking=no -o BatchMode=yes -i %s /tmp/perf-stat.sh ubuntu@%s:/tmp/perf-stat.sh 2>/dev/null",
			serverKeyPath, serverIP),
		"",
		"# Start profile capture on server (blocks ~20s).",
		fmt.Sprintf("%s 'bash /tmp/profile-capture.sh' &", sshBase),
		"PROF_PID=$!",
		"",
		"# Start perf stat on server (blocks ~15s).",
		fmt.Sprintf("%s 'bash /tmp/perf-stat.sh' &", sshBase),
		"PERF_PID=$!",
		"",
		"# Run load from client (25s).",
		"sleep 2",
		loadCmd,
		"",
		"# Wait for profile and perf captures.",
		"wait $PROF_PID 2>/dev/null || true",
		"wait $PERF_PID 2>/dev/null || true",
		"",
		"# Show perf stat results.",
		fmt.Sprintf("%s 'cat %s/perf-stat.txt 2>/dev/null || echo no_perf_stat'", sshBase, remoteOutputDir),
		"",
		"# Kill server.",
		fmt.Sprintf("%s 'sudo pkill -9 -f profiled 2>/dev/null; true'", sshBase),
		"sleep 2",
	}, "\n")
}

// analyzeProfiles runs automated analysis on captured profiles using go tool pprof.
// Generates a text report with top CPU hotspots, allocation hotspots, main-vs-branch
// diffs, and perf stat summaries.
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
