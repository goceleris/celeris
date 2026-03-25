//go:build mage

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CloudAdaptiveTest runs a dedicated adaptive engine stress test on AWS EC2.
// It starts the server in adaptive mode for each protocol (h1, h2, hybrid)
// and applies synthetic load patterns to trigger engine switches, measuring
// throughput, switch events, and recovery behavior.
//
// The test captures server logs to track switch events (the adaptive engine
// logs "engine switch completed" with the new active engine).
//
// Set CLOUD_ARCH=amd64|arm64 (default: arm64).
func CloudAdaptiveTest() error {
	if err := awsEnsureCLI(); err != nil {
		return err
	}

	dir, err := resultsDir("cloud-adaptive-test")
	if err != nil {
		return err
	}

	arches := cloudArch()
	if len(arches) > 1 {
		arches = []string{arches[0]}
	}
	arch := arches[0]
	instanceType := awsInstanceType(arch)

	fmt.Printf("Cloud Adaptive Test (%s on %s)\n\n", arch, instanceType)

	// Build server binary.
	serverBin := filepath.Join(dir, fmt.Sprintf("server-%s", arch))
	fmt.Println("Building server...")
	if err := crossCompile("test/benchmark/server", serverBin, arch); err != nil {
		return fmt.Errorf("build server: %w", err)
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
	_, _ = awsSSH(serverPublicIP, keyPath, "mkdir -p /tmp/adaptive")
	if err := awsSCP(serverBin, "/tmp/adaptive/server", serverPublicIP, keyPath); err != nil {
		return err
	}
	_, _ = awsSSH(serverPublicIP, keyPath, "chmod +x /tmp/adaptive/server")

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

	if _, err := awsSSH(clientPublicIP, keyPath, "sudo apt-get update -qq && sudo apt-get install -y -qq wrk nghttp2-client"); err != nil {
		return fmt.Errorf("install tools: %w", err)
	}
	if err := awsSCP(keyPath, "/tmp/server-key.pem", clientPublicIP, keyPath); err != nil {
		return err
	}
	_, _ = awsSSH(clientPublicIP, keyPath, "chmod 600 /tmp/server-key.pem")


	// Run adaptive test for each protocol.
	for _, proto := range []string{"h1", "h2", "hybrid"} {
		fmt.Printf("\n%s\n", strings.Repeat("=", 70))
		fmt.Printf("ADAPTIVE TEST: protocol=%s\n", proto)
		fmt.Printf("%s\n\n", strings.Repeat("=", 70))

		// Start server in adaptive mode (directly on server, not via client hop).
		_, _ = awsSSH(serverPublicIP, keyPath, "sudo pkill -9 -f server 2>/dev/null; sleep 1")
		startCmd := fmt.Sprintf("sudo prlimit --memlock=unlimited env ENGINE=adaptive PROTOCOL=%s PORT=18080 /tmp/adaptive/server > /tmp/adaptive/server-%s.log 2>&1 &",
			proto, proto)
		_, _ = awsSSH(serverPublicIP, keyPath, "bash -c '"+startCmd+"'")
		time.Sleep(4 * time.Second)

		// Verify server is up.
		checkOut, _ := awsSSH(serverPublicIP, keyPath,
			fmt.Sprintf("head -10 /tmp/adaptive/server-%s.log", proto))
		fmt.Printf("Server startup:\n%s\n", checkOut)

		// Phase 1: Steady-state warmup (30s).
		fmt.Println("--- Phase 1: Steady-state warmup (30s) ---")
		rps := runAdaptiveLoad(clientPublicIP, keyPath, serverPrivateIP, proto, "30s", "256")
		fmt.Printf("  Warmup RPS: %s\n", rps)

		// Phase 2: High concurrency burst (15s with 4096 connections).
		fmt.Println("--- Phase 2: High concurrency burst (15s, 4096 conns) ---")
		rps = runAdaptiveLoad(clientPublicIP, keyPath, serverPrivateIP, proto, "15s", "4096")
		fmt.Printf("  Burst RPS: %s\n", rps)

		// Phase 3: Low concurrency recovery (15s with 32 connections).
		fmt.Println("--- Phase 3: Low concurrency recovery (15s, 32 conns) ---")
		rps = runAdaptiveLoad(clientPublicIP, keyPath, serverPrivateIP, proto, "15s", "32")
		fmt.Printf("  Recovery RPS: %s\n", rps)

		// Phase 4: Steady state again (30s).
		fmt.Println("--- Phase 4: Steady state (30s) ---")
		rps = runAdaptiveLoad(clientPublicIP, keyPath, serverPrivateIP, proto, "30s", "256")
		fmt.Printf("  Steady RPS: %s\n", rps)

		// Phase 5: Wait for eval cycles to potentially trigger switch (60s idle + 15s load).
		fmt.Println("--- Phase 5: Idle period (60s) then reload ---")
		time.Sleep(60 * time.Second)
		rps = runAdaptiveLoad(clientPublicIP, keyPath, serverPrivateIP, proto, "15s", "256")
		fmt.Printf("  Post-idle RPS: %s\n", rps)

		// Collect server logs for switch analysis.
		fmt.Println("\n--- Server log analysis ---")
		logOut, _ := awsSSH(serverPublicIP, keyPath,
			fmt.Sprintf("cat /tmp/adaptive/server-%s.log", proto))

		// Extract key events.
		switchCount := 0
		for _, line := range strings.Split(logOut, "\n") {
			if strings.Contains(line, "engine switch") || strings.Contains(line, "switch recommended") ||
				strings.Contains(line, "oscillation") || strings.Contains(line, "adaptive") {
				fmt.Printf("  %s\n", strings.TrimSpace(line))
				if strings.Contains(line, "switch completed") {
					switchCount++
				}
			}
		}
		fmt.Printf("\n  Total switches: %d\n", switchCount)

		// Stop server.
		_, _ = awsSSH(serverPublicIP, keyPath, "sudo pkill -9 -f server 2>/dev/null")
		time.Sleep(2 * time.Second)

		// Save full log.
		logPath := filepath.Join(dir, fmt.Sprintf("server-%s.log", proto))
		_ = os.WriteFile(logPath, []byte(logOut), 0o644)
	}

	fmt.Printf("\nResults saved to: %s\n", dir)
	return nil
}

// runAdaptiveLoad runs a load test and returns the RPS string.
func runAdaptiveLoad(clientIP, keyPath, serverIP, proto, duration, conns string) string {
	var loadCmd string
	if proto == "h2" {
		c, _ := strconv.Atoi(conns)
		if c > 128 {
			c = 128
		}
		loadCmd = fmt.Sprintf("h2load -c%d -m128 -t4 -D %s http://%s:18080/", c, strings.TrimSuffix(duration, "s"), serverIP)
	} else {
		loadCmd = fmt.Sprintf("wrk -t4 -c%s -d%s --latency http://%s:18080/", conns, duration, serverIP)
	}

	out, _ := awsSSH(clientIP, keyPath, loadCmd)

	// Extract RPS.
	if proto == "h2" {
		if m := h2loadRPSRegex.FindStringSubmatch(out); m != nil {
			return m[1]
		}
	} else {
		if m := wrkRPSRegex.FindStringSubmatch(out); m != nil {
			return m[1]
		}
	}
	return "0 (failed)"
}
