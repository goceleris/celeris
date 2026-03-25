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

// CloudIOUringSweep runs a comprehensive io_uring vs epoll comparison across
// connection counts, protocols, worker counts, and request patterns on
// separate EC2 instances.
//
// The sweep tests:
// - Connection counts: 16, 64, 256, 1024, 4096, 16384, 32768, 65536
// - Protocols: h1, h2
// - Worker counts: default (GOMAXPROCS), 1 (single-threaded)
// - Engines: iouring, epoll (interleaved per config for fairness)
//
// Set CLOUD_ARCH=amd64|arm64|both (default: both).
// Set SWEEP_INSTANCE=<type> to override instance type (default: c7g.16xlarge for arm64, c7i.16xlarge for x86).
// Set SWEEP_DURATION=10 (default) seconds per run.
// Set SWEEP_CONNS=16,64,256,1024,4096,16384,32768,65536 to override connection counts.
func CloudIOUringSweep() error {
	if err := awsEnsureCLI(); err != nil {
		return err
	}

	dir, err := resultsDir("cloud-iouring-sweep")
	if err != nil {
		return err
	}

	arches := cloudArch()
	duration := metalEnvOr("SWEEP_DURATION", "10")

	connStrs := strings.Split(metalEnvOr("SWEEP_CONNS", "16,64,256,1024,4096,16384,32768,65536"), ",")
	var connCounts []int
	for _, s := range connStrs {
		c, _ := strconv.Atoi(strings.TrimSpace(s))
		if c > 0 {
			connCounts = append(connCounts, c)
		}
	}

	protocols := []string{"h1", "h2"}
	engines := []string{"epoll", "iouring"}
	workerModes := []string{"default", "1"}

	fmt.Printf("Cloud io_uring Sweep\n")
	fmt.Printf("Architectures: %v\n", arches)
	fmt.Printf("Connections: %v\n", connCounts)
	fmt.Printf("Protocols: %v, Workers: %v\n", protocols, workerModes)
	fmt.Printf("Duration: %ss per run\n\n", duration)

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

	for _, arch := range arches {
		fmt.Printf("\n%s\n", strings.Repeat("=", 80))
		fmt.Printf("ARCHITECTURE: %s\n", arch)
		fmt.Printf("%s\n\n", strings.Repeat("=", 80))

		// Use large instances to have many cores and high connection capacity.
		instanceType := metalEnvOr("SWEEP_INSTANCE", sweepInstanceType(arch))

		// Build server.
		serverBin := filepath.Join(dir, fmt.Sprintf("server-%s", arch))
		fmt.Printf("Building server for %s...\n", arch)
		if err := crossCompile("test/benchmark/server", serverBin, arch); err != nil {
			return fmt.Errorf("build server (%s): %w", arch, err)
		}

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
		fmt.Printf("  Server: %s (private: %s) [%s]\n", serverPublicIP, serverPrivateIP, instanceType)

		_, _ = awsSSH(serverPublicIP, keyPath, "echo 0 | sudo tee /proc/sys/kernel/apparmor_restrict_unprivileged_io_uring > /dev/null 2>&1")
		_, _ = awsSSH(serverPublicIP, keyPath, "mkdir -p /tmp/sweep")
		if err := awsSCP(serverBin, "/tmp/sweep/server", serverPublicIP, keyPath); err != nil {
			return err
		}
		_, _ = awsSSH(serverPublicIP, keyPath, "chmod +x /tmp/sweep/server")

		// Print kernel version.
		kernelOut, _ := awsSSH(serverPublicIP, keyPath, "uname -r")
		fmt.Printf("  Kernel: %s", kernelOut)

		// Print CPU info.
		cpuOut, _ := awsSSH(serverPublicIP, keyPath, "nproc")
		fmt.Printf("  CPUs: %s", cpuOut)

		// Launch client — use same instance type for balanced load generation.
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

		// Increase client ulimits for high connection counts.
		_, _ = awsSSH(clientPublicIP, keyPath, "sudo sysctl -w net.ipv4.ip_local_port_range='1024 65535' && ulimit -n 131072 2>/dev/null")

		var allResults []sweepResult

		totalRuns := len(protocols) * len(connCounts) * len(engines) * len(workerModes)
		run := 0

		for _, workers := range workerModes {
			for _, proto := range protocols {
				for _, conns := range connCounts {
					for _, eng := range engines {
						run++
						fmt.Printf("[%d/%d] %s %s %dc w=%s: ", run, totalRuns, eng, proto, conns, workers)

						// Start server.
						_, _ = awsSSH(serverPublicIP, keyPath, "sudo pkill -9 -f server 2>/dev/null; sleep 1")
						workersEnv := ""
						if workers != "default" {
							workersEnv = fmt.Sprintf("WORKERS=%s ", workers)
						}
						startCmd := fmt.Sprintf("sudo prlimit --memlock=unlimited %senv ENGINE=%s PROTOCOL=%s PORT=18080 /tmp/sweep/server > /tmp/sweep/server.log 2>&1 &",
							workersEnv, eng, proto)
						_, _ = awsSSH(serverPublicIP, keyPath, "bash -c '"+startCmd+"'")
						time.Sleep(3 * time.Second)

						// Run load.
						var loadCmd string
						if proto == "h2" {
							h2c := conns
							if h2c > 256 {
								h2c = 256
							}
							streams := 128
							loadCmd = fmt.Sprintf("h2load -c%d -m%d -t4 -D %s http://%s:18080/",
								h2c, streams, duration, serverPrivateIP)
						} else {
							loadCmd = fmt.Sprintf("wrk -t4 -c%d -d%ss --latency http://%s:18080/",
								conns, duration, serverPrivateIP)
						}

						out, _ := awsSSH(clientPublicIP, keyPath, loadCmd)

						var rps float64
						if proto == "h2" {
							if m := h2loadRPSRegex.FindStringSubmatch(out); m != nil {
								rps, _ = strconv.ParseFloat(m[1], 64)
							}
						} else {
							if m := wrkRPSRegex.FindStringSubmatch(out); m != nil {
								rps, _ = strconv.ParseFloat(m[1], 64)
							}
						}

						allResults = append(allResults, sweepResult{proto, eng, workers, conns, rps})
						fmt.Printf("%.0f rps\n", rps)

						_, _ = awsSSH(serverPublicIP, keyPath, "sudo pkill -9 -f server 2>/dev/null")
						time.Sleep(2 * time.Second)
					}
				}
			}
		}

		// Generate report for this architecture.
		report := generateSweepReport(arch, instanceType, kernelOut, cpuOut, duration, protocols, connCounts, engines, workerModes, allResults)
		fmt.Println("\n" + report)

		reportPath := filepath.Join(dir, fmt.Sprintf("report-%s.txt", arch))
		_ = os.WriteFile(reportPath, []byte(report), 0o644)
	}

	return nil
}

func sweepInstanceType(arch string) string {
	if arch == "arm64" {
		return "c7g.16xlarge" // 64 vCPU Graviton3
	}
	return "c7i.16xlarge" // 64 vCPU Intel
}

type sweepResult struct {
	proto, engine, workers string
	conns                  int
	rps                    float64
}

func generateSweepReport(arch, instance, kernel, cpus, duration string, protocols []string, connCounts []int, engines, workerModes []string, results []sweepResult) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("io_uring vs epoll Sweep — %s\n", arch))
	sb.WriteString(fmt.Sprintf("Instance: %s, Kernel: %s, CPUs: %s", instance, strings.TrimSpace(kernel), strings.TrimSpace(cpus)))
	sb.WriteString(fmt.Sprintf("Duration: %ss per run\n\n", duration))

	// Build lookup.
	lookup := make(map[string]float64)
	for _, r := range results {
		key := fmt.Sprintf("%s|%s|%s|%d", r.proto, r.engine, r.workers, r.conns)
		lookup[key] = r.rps
	}

	for _, workers := range workerModes {
		wLabel := "default"
		if workers == "1" {
			wLabel = "single-worker"
		}

		for _, proto := range protocols {
			sb.WriteString(fmt.Sprintf("--- %s / %s workers ---\n", strings.ToUpper(proto), wLabel))
			sb.WriteString(fmt.Sprintf("%-10s | %14s | %14s | %10s\n", "Conns", "io_uring", "epoll", "iu adv."))
			sb.WriteString(strings.Repeat("-", 56) + "\n")

			for _, conns := range connCounts {
				iouKey := fmt.Sprintf("%s|iouring|%s|%d", proto, workers, conns)
				epoKey := fmt.Sprintf("%s|epoll|%s|%d", proto, workers, conns)
				iou := lookup[iouKey]
				epo := lookup[epoKey]
				adv := "N/A"
				if epo > 0 {
					adv = fmt.Sprintf("%+.1f%%", (iou-epo)/epo*100)
				}
				sb.WriteString(fmt.Sprintf("%-10d | %14.0f | %14.0f | %10s\n", conns, iou, epo, adv))
			}
			sb.WriteString("\n")
		}
	}

	// Summary: find io_uring's best showing.
	sb.WriteString("--- Summary: io_uring best advantages ---\n")
	type advantage struct {
		proto, workers string
		conns          int
		pct            float64
	}
	var best []advantage
	for _, workers := range workerModes {
		for _, proto := range protocols {
			for _, conns := range connCounts {
				iouKey := fmt.Sprintf("%s|iouring|%s|%d", proto, workers, conns)
				epoKey := fmt.Sprintf("%s|epoll|%s|%d", proto, workers, conns)
				iou := lookup[iouKey]
				epo := lookup[epoKey]
				if epo > 0 {
					pct := (iou - epo) / epo * 100
					best = append(best, advantage{proto, workers, conns, pct})
				}
			}
		}
	}
	// Sort by advantage descending.
	for i := 0; i < len(best); i++ {
		for j := i + 1; j < len(best); j++ {
			if best[j].pct > best[i].pct {
				best[i], best[j] = best[j], best[i]
			}
		}
	}
	for i, a := range best {
		if i >= 5 {
			break
		}
		sb.WriteString(fmt.Sprintf("  %s %dc w=%s: %+.1f%%\n", a.proto, a.conns, a.workers, a.pct))
	}

	sb.WriteString("\n--- Summary: epoll best advantages ---\n")
	for i := len(best) - 1; i >= 0 && i >= len(best)-5; i-- {
		a := best[i]
		sb.WriteString(fmt.Sprintf("  %s %dc w=%s: %+.1f%%\n", a.proto, a.conns, a.workers, a.pct))
	}

	return sb.String()
}
