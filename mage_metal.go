//go:build mage

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// benchRepoRelPath is the relative path to the benchmarks repo (sibling of celeris).
const benchRepoRelPath = "../benchmarks"

// metalServerConfigs defines all celeris server configurations to test.
// Format: engine-objective-protocol-preset
// This tests all engine×objective×protocol combos with both Greedy and Minimal presets.
var metalServerConfigs = func() []metalConfig {
	var configs []metalConfig
	for _, eng := range []string{"iouring", "epoll", "adaptive"} {
		for _, obj := range []string{"latency", "throughput", "balanced"} {
			for _, proto := range []string{"h1", "h2", "hybrid"} {
				for _, preset := range []string{"greedy", "minimal"} {
					configs = append(configs, metalConfig{
						engine: eng, objective: obj, protocol: proto, preset: preset,
					})
				}
			}
		}
	}
	return configs
}()

type metalConfig struct {
	engine, objective, protocol, preset string
}

func (c metalConfig) name() string {
	return fmt.Sprintf("%s-%s-%s-%s", c.engine, c.objective, c.protocol, c.preset)
}

func (c metalConfig) envVars(port string) string {
	return fmt.Sprintf("ENGINE=%s OBJECTIVE=%s PROTOCOL=%s PORT=%s PRESET=%s",
		c.engine, c.objective, c.protocol, port, c.preset)
}

// CloudMetalBenchmark runs the full celeris benchmark matrix using the benchmarks
// repo's custom H1/H2 clients on separate EC2 instances. Tests all 54 configs
// (3 engines × 3 objectives × 3 protocols × 2 presets) across up to 3 celeris
// versions (v1.0.0, HEAD, current working tree).
//
// Server binaries are built from test/benchmark/server/ which uses resource.Config
// directly (not the public celeris.Config API) for full preset control.
//
// Published versions (v1.0.0, tags) are fetched via the Go module proxy — no
// worktree or local builds needed. Only "current" (uncommitted changes) uses
// the local working tree.
//
// Set CLOUD_ARCH=amd64|arm64 (default: arm64).
// Set METAL_INSTANCE=c7g.4xlarge (default: same as awsInstanceType).
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

	fmt.Printf("Cloud Metal Benchmark: %s (%s on %s)\n", branch, arch, instanceType)
	fmt.Printf("Refs: %v\n", refs)
	fmt.Printf("Configs: %d (3 engines × 3 objectives × 3 protocols × 2 presets)\n", len(metalServerConfigs))
	fmt.Printf("Duration: %s per benchmark\n\n", duration)

	// Step 1: Build server binaries for each ref.
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

	// Step 2: Build client binary from benchmarks repo.
	benchRepoAbs, err := filepath.Abs(benchRepoRelPath)
	if err != nil {
		return fmt.Errorf("resolve benchmarks repo: %w", err)
	}
	if _, err := os.Stat(filepath.Join(benchRepoAbs, "go.mod")); err != nil {
		return fmt.Errorf("benchmarks repo not found at %s — clone it as a sibling: %w", benchRepoAbs, err)
	}

	clientBin := filepath.Join(dir, fmt.Sprintf("bench-%s", arch))
	fmt.Println("Building benchmark client...")
	if err := metalCrossCompile(benchRepoAbs, "cmd/bench", clientBin, arch); err != nil {
		return fmt.Errorf("build bench client: %w", err)
	}

	// Step 3: Launch EC2 instances.
	keyName, keyPath, err := awsCreateKeyPair(dir)
	if err != nil {
		return err
	}
	sgID, err := awsCreateSecurityGroup()
	if err != nil {
		awsDeleteKeyPair(keyName)
		return err
	}
	// Allow server + control ports.
	for _, port := range []string{"8080", "9999", "18080"} {
		_, _ = awsCLI("ec2", "authorize-security-group-ingress",
			"--region", awsRegion, "--group-id", sgID,
			"--protocol", "tcp", "--port", port, "--source-group", sgID)
	}

	var allInstanceIDs []string
	defer func() {
		awsCleanup(allInstanceIDs, keyName, sgID, keyPath)
	}()

	ami, err := awsLatestAMI(arch)
	if err != nil {
		return err
	}

	// Launch server instance.
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

	// Configure server.
	_, _ = awsSSH(serverPublicIP, keyPath, "echo 0 | sudo tee /proc/sys/kernel/apparmor_restrict_unprivileged_io_uring > /dev/null 2>&1")
	_, _ = awsSSH(serverPublicIP, keyPath, "mkdir -p /tmp/metal")

	// Upload server binaries.
	for ref, bin := range serverBins {
		label := sanitizeRef(ref)
		remote := fmt.Sprintf("/tmp/metal/server-%s", label)
		if err := awsSCP(bin, remote, serverPublicIP, keyPath); err != nil {
			return fmt.Errorf("upload server %s: %w", ref, err)
		}
	}
	_, _ = awsSSH(serverPublicIP, keyPath, "chmod +x /tmp/metal/server-*")

	// Launch client instance.
	clientID, clientPublicIP, err := awsLaunchInstance(ami, instanceType, keyName, sgID, arch)
	if err != nil {
		return err
	}
	allInstanceIDs = append(allInstanceIDs, clientID)
	if err := awsWaitSSH(clientPublicIP, keyPath); err != nil {
		return err
	}
	fmt.Printf("  Client: %s\n", clientPublicIP)

	// Upload client binary + SSH key.
	_, _ = awsSSH(clientPublicIP, keyPath, "mkdir -p /tmp/metal/results")
	if err := awsSCP(clientBin, "/tmp/metal/bench", clientPublicIP, keyPath); err != nil {
		return err
	}
	_, _ = awsSSH(clientPublicIP, keyPath, "chmod +x /tmp/metal/bench")
	if err := awsSCP(keyPath, "/tmp/server-key.pem", clientPublicIP, keyPath); err != nil {
		return err
	}
	_, _ = awsSSH(clientPublicIP, keyPath, "chmod 600 /tmp/server-key.pem")

	// Install wrk + h2load on client for fallback.
	_, _ = awsSSH(clientPublicIP, keyPath, "sudo apt-get update -qq && sudo apt-get install -y -qq wrk nghttp2-client 2>/dev/null")

	sshToServer := fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o BatchMode=yes -i /tmp/server-key.pem ubuntu@%s", serverPrivateIP)

	// Step 4: Run benchmarks for each ref — interleaved per config for fairness.
	for i, cfg := range metalServerConfigs {
		fmt.Printf("\n--- Config %d/%d: %s ---\n", i+1, len(metalServerConfigs), cfg.name())

		for _, ref := range refs {
			label := sanitizeRef(ref)
			serverBinRemote := fmt.Sprintf("/tmp/metal/server-%s", label)

			// Start server with this config.
			startScript := fmt.Sprintf(`#!/bin/bash
%s 'sudo pkill -9 -f server- 2>/dev/null || true'
sleep 1
%s 'sudo prlimit --memlock=unlimited env %s %s > /tmp/metal/server.log 2>&1 &'
sleep 3
# Check server is listening
if ! %s 'timeout 2 bash -c "echo > /dev/tcp/localhost/18080" 2>/dev/null'; then
  echo "ERROR: server not listening"
  %s 'cat /tmp/metal/server.log' || true
fi
`, sshToServer, sshToServer, cfg.envVars("18080"), serverBinRemote, sshToServer, sshToServer)

			scriptPath := filepath.Join(dir, fmt.Sprintf("start-%s-%s.sh", cfg.name(), label))
			_ = os.WriteFile(scriptPath, []byte(startScript), 0o755)
			if err := awsSCP(scriptPath, "/tmp/metal/start.sh", clientPublicIP, keyPath); err != nil {
				continue
			}
			startOut, err := awsSSH(clientPublicIP, keyPath, "bash /tmp/metal/start.sh")
			if err != nil {
				fmt.Printf("  %s: server start failed: %v\n", label, err)
				continue
			}
			if strings.Contains(startOut, "ERROR") {
				fmt.Printf("  %s: %s\n", label, startOut)
				continue
			}

			// Determine load tool based on protocol.
			var loadCmd string
			switch cfg.protocol {
			case "h2":
				loadCmd = fmt.Sprintf("h2load -c128 -m128 -t4 -D %s http://%s:18080/", duration, serverPrivateIP)
			default:
				loadCmd = fmt.Sprintf("wrk -t4 -c256 -d%s --latency http://%s:18080/", duration, serverPrivateIP)
			}

			fmt.Printf("  %s: ", label)
			loadOut, _ := awsSSH(clientPublicIP, keyPath, loadCmd)

			// Extract RPS from output.
			rps := "?"
			if cfg.protocol == "h2" {
				if m := h2loadRPSRegex.FindStringSubmatch(loadOut); m != nil {
					rps = m[1]
				}
			} else {
				if m := wrkRPSRegex.FindStringSubmatch(loadOut); m != nil {
					rps = m[1]
				}
			}
			fmt.Printf("%s rps\n", rps)

			// Stop server.
			_, _ = awsSSH(clientPublicIP, keyPath, fmt.Sprintf("%s 'sudo pkill -9 -f server- 2>/dev/null || true'", sshToServer))
			time.Sleep(2 * time.Second)
		}
	}

	fmt.Printf("\nResults saved to: %s\n", dir)
	return nil
}

// buildMetalServer builds the test/benchmark/server binary for a specific
// celeris ref. For published versions, uses the Go module proxy. For "current",
// uses the local working tree.
func buildMetalServer(ref, outputPath, arch string) error {
	if ref == "current" {
		// Build from current working tree.
		return crossCompile("test/benchmark/server", outputPath, arch)
	}

	// Build from a specific ref using git worktree.
	return crossCompileFromRef(ref, "test/benchmark/server", outputPath, arch)
}

// metalCrossCompile builds a package from an external directory.
func metalCrossCompile(repoDir, pkg, outputPath, arch string) error {
	absOutput, err := filepath.Abs(outputPath)
	if err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-o", absOutput, "./"+pkg)
	cmd.Dir = repoDir
	cmd.Env = append(os.Environ(),
		"GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0",
	)
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
