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

// CloudIOUringSweep runs a systematic comparison of io_uring vs epoll across
// a wide range of connection counts, protocols, and request patterns to find
// where io_uring has a measurable advantage over epoll.
//
// Sweeps:
// - Connection counts: 16, 64, 256, 1024, 4096, 16384
// - Protocols: h1, h2
// - Engines: iouring, epoll
//
// Each combination runs for 10s with a 3s warmup. Results are collected
// inline and presented as a comparison table showing the io_uring advantage
// (or disadvantage) at each point.
//
// Set CLOUD_ARCH=amd64|arm64 (default: arm64).
func CloudIOUringSweep() error {
	if err := awsEnsureCLI(); err != nil {
		return err
	}

	dir, err := resultsDir("cloud-iouring-sweep")
	if err != nil {
		return err
	}

	arches := cloudArch()
	if len(arches) > 1 {
		arches = []string{arches[0]}
	}
	arch := arches[0]
	instanceType := awsInstanceType(arch)

	connCounts := []int{16, 64, 256, 1024, 4096, 16384}
	protocols := []string{"h1", "h2"}
	engines := []string{"iouring", "epoll"}
	duration := "10"

	totalRuns := len(connCounts) * len(protocols) * len(engines)
	fmt.Printf("Cloud io_uring Sweep (%s on %s)\n", arch, instanceType)
	fmt.Printf("Connections: %v\n", connCounts)
	fmt.Printf("Protocols: %v\n", protocols)
	fmt.Printf("Total runs: %d\n\n", totalRuns)

	// Build server.
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
	_, _ = awsSSH(serverPublicIP, keyPath, "mkdir -p /tmp/sweep")
	if err := awsSCP(serverBin, "/tmp/sweep/server", serverPublicIP, keyPath); err != nil {
		return err
	}
	_, _ = awsSSH(serverPublicIP, keyPath, "chmod +x /tmp/sweep/server")

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

	// Results: map[proto-conns][engine] = rps
	type sweepKey struct {
		proto string
		conns int
	}
	results := make(map[sweepKey]map[string]float64)

	run := 0
	for _, proto := range protocols {
		for _, conns := range connCounts {
			key := sweepKey{proto, conns}
			results[key] = make(map[string]float64)

			// Interleave engines for fairness (epoll first, then iouring, to cancel thermal drift).
			for _, eng := range engines {
				run++
				fmt.Printf("[%d/%d] %s %s %d conns: ", run, totalRuns, eng, proto, conns)

				// Start server.
				_, _ = awsSSH(serverPublicIP, keyPath, "sudo pkill -9 -f server 2>/dev/null; sleep 1")
				startCmd := fmt.Sprintf("sudo prlimit --memlock=unlimited env ENGINE=%s PROTOCOL=%s PORT=18080 /tmp/sweep/server > /tmp/sweep/server.log 2>&1 &",
					eng, proto)
				_, _ = awsSSH(serverPublicIP, keyPath, "bash -c '"+startCmd+"'")
				time.Sleep(3 * time.Second)

				// Run load.
				var loadCmd string
				if proto == "h2" {
					h2c := conns
					if h2c > 128 {
						h2c = 128
					}
					streams := 128
					if conns <= 128 {
						streams = conns
					}
					loadCmd = fmt.Sprintf("h2load -c%d -m%d -t4 -D %s http://%s:18080/",
						h2c, streams, duration, serverPrivateIP)
				} else {
					loadCmd = fmt.Sprintf("wrk -t4 -c%d -d%ss --latency http://%s:18080/",
						conns, duration, serverPrivateIP)
				}

				out, _ := awsSSH(clientPublicIP, keyPath, loadCmd)

				// Parse RPS.
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

				results[key][eng] = rps
				fmt.Printf("%.0f rps\n", rps)

				// Stop server.
				_, _ = awsSSH(serverPublicIP, keyPath, "sudo pkill -9 -f server 2>/dev/null")
				time.Sleep(2 * time.Second)
			}
		}
	}

	// Generate report.
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("io_uring vs epoll Sweep Report (%s on %s)\n", arch, instanceType))
	sb.WriteString(fmt.Sprintf("Date: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("Duration: %ss per run\n\n", duration))

	for _, proto := range protocols {
		sb.WriteString(fmt.Sprintf("--- %s ---\n", strings.ToUpper(proto)))
		sb.WriteString(fmt.Sprintf("%-10s | %14s | %14s | %10s\n", "Conns", "io_uring", "epoll", "iu adv."))
		sb.WriteString(strings.Repeat("-", 55) + "\n")

		for _, conns := range connCounts {
			key := sweepKey{proto, conns}
			iou := results[key]["iouring"]
			epo := results[key]["epoll"]
			adv := ""
			if epo > 0 {
				advPct := (iou - epo) / epo * 100
				adv = fmt.Sprintf("%+.1f%%", advPct)
			}
			sb.WriteString(fmt.Sprintf("%-10d | %14.0f | %14.0f | %10s\n", conns, iou, epo, adv))
		}
		sb.WriteString("\n")
	}

	// Find crossover point.
	sb.WriteString("--- Analysis ---\n")
	for _, proto := range protocols {
		bestIOU := 0
		for _, conns := range connCounts {
			key := sweepKey{proto, conns}
			iou := results[key]["iouring"]
			epo := results[key]["epoll"]
			if iou > epo*1.02 { // 2% threshold
				if bestIOU == 0 {
					sb.WriteString(fmt.Sprintf("%s: io_uring first leads at %d connections (+%.1f%%)\n",
						proto, conns, (iou-epo)/epo*100))
				}
				bestIOU = conns
			}
		}
		if bestIOU == 0 {
			sb.WriteString(fmt.Sprintf("%s: epoll leads or ties at all connection counts\n", proto))
		}
	}

	report := sb.String()
	fmt.Println("\n" + report)

	reportPath := filepath.Join(dir, fmt.Sprintf("report-%s.txt", arch))
	_ = os.WriteFile(reportPath, []byte(report), 0o644)
	fmt.Printf("Report saved to: %s\n", reportPath)

	return nil
}
