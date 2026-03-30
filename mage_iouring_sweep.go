//go:build mage

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CloudEngineAnalysis runs a comprehensive io_uring vs epoll characterization
// across 4 distinct workload patterns, 7 connection counts, 2 protocols, and
// 2 engines. Each run collects throughput, latency percentiles (H1) or
// mean/max/sd (H2), server CPU%, and peak RSS.
//
// Workloads:
//   - short-lived:        Connection: close (one request per TCP connection)
//   - keep-alive:         Persistent connections (standard benchmark)
//   - large-download:     64KB response body with Connection: close
//   - sustained-download: 64KB response body with keep-alive
//
// Set CLOUD_ARCH=amd64|arm64|both (default: both).
// Set ANALYSIS_INSTANCE=<type> to override instance type.
// Set ANALYSIS_DURATION=15 (default) seconds per run.
// Set ANALYSIS_CONNS=64,256,1024,4096 to override connection counts.
func CloudEngineAnalysis() error {
	if err := awsEnsureCLI(); err != nil {
		return err
	}

	dir, err := resultsDir("cloud-engine-analysis")
	if err != nil {
		return err
	}

	arches := cloudArch()
	duration := metalEnvOr("ANALYSIS_DURATION", "15")

	connStrs := strings.Split(metalEnvOr("ANALYSIS_CONNS", "16,64,256,1024,4096,16384,64000"), ",")
	var connCounts []int
	for _, s := range connStrs {
		c, _ := strconv.Atoi(strings.TrimSpace(s))
		if c > 0 {
			connCounts = append(connCounts, c)
		}
	}

	numRuns, _ := strconv.Atoi(metalEnvOr("ANALYSIS_RUNS", "1"))
	if numRuns < 1 {
		numRuns = 1
	}

	workloads := []analysisWorkload{
		{name: "short-lived", path: "/", connClose: true},
		{name: "keep-alive", path: "/", connClose: false},
		{name: "large-download", path: "/download", connClose: true},
		{name: "sustained-download", path: "/download", connClose: false},
	}
	protocols := []string{"h1", "h2"}
	engines := []string{"epoll", "iouring"}

	configsPerRun := len(workloads) * len(connCounts) * len(protocols) * len(engines)
	totalPerArch := configsPerRun * numRuns
	fmt.Printf("Cloud Engine Analysis\n")
	fmt.Printf("Architectures: %v\n", arches)
	fmt.Printf("Workloads: %v\n", workloadNames(workloads))
	fmt.Printf("Connections: %v\n", connCounts)
	fmt.Printf("Protocols: %v, Engines: %v\n", protocols, engines)
	fmt.Printf("Duration: %ss per config, %d configs × %d run(s) = %d per arch, %d total\n\n",
		duration, configsPerRun, numRuns, totalPerArch, totalPerArch*len(arches))

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

		instanceType := metalEnvOr("ANALYSIS_INSTANCE", analysisInstanceType(arch))

		serverBin := filepath.Join(dir, fmt.Sprintf("server-%s", arch))
		fmt.Printf("Building server for %s...\n", arch)
		if err := crossCompile("test/benchmark/server", serverBin, arch); err != nil {
			return fmt.Errorf("build server (%s): %w", arch, err)
		}

		loadgenBin := filepath.Join(dir, fmt.Sprintf("loadgen-%s", arch))
		fmt.Printf("Building loadgen for %s...\n", arch)
		loadgenRepoPath := filepath.Join("..", "loadgen")
		if err := metalCrossCompile(loadgenRepoPath, "cmd/loadgen", loadgenBin, arch); err != nil {
			return fmt.Errorf("build loadgen (%s): %w", arch, err)
		}

		// Use AL2023 kernel 6.18 AMI for server when MAINLINE_KERNEL=1,
		// otherwise standard Ubuntu 24.04.
		useAL2023 := os.Getenv("MAINLINE_KERNEL") == "1"
		sshUser := "ubuntu"
		if useAL2023 {
			sshUser = "ec2-user"
		}
		var serverAMI, clientAMI string

		if useAL2023 {
			// AL2023 with kernel 6.18 LTS for server (io_uring testing).
			ssmParam := fmt.Sprintf("/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-6.18-%s", arch)
			out, err := awsCLI("ssm", "get-parameter", "--region", awsRegion,
				"--name", ssmParam, "--query", "Parameter.Value", "--output", "text")
			if err != nil {
				return fmt.Errorf("resolve AL2023 6.18 AMI: %w", err)
			}
			serverAMI = strings.TrimSpace(out)
			fmt.Printf("  AL2023 kernel 6.18 AMI: %s\n", serverAMI)
		}

		ubuntuAMI, err := awsLatestAMI(arch)
		if err != nil {
			return err
		}
		if !useAL2023 {
			serverAMI = ubuntuAMI
		}
		clientAMI = ubuntuAMI

		// Launch server.
		serverID, serverPublicIP, err := awsLaunchInstance(serverAMI, instanceType, keyName, sgID, arch)
		if err != nil {
			return err
		}
		allInstanceIDs = append(allInstanceIDs, serverID)
		if err := awsWaitSSHAs(sshUser, serverPublicIP, keyPath); err != nil {
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

		// Server SSH helper — uses the right user (ec2-user for AL2023, ubuntu for Ubuntu).
		serverSSH := func(cmd string) (string, error) {
			return awsSSHAs(sshUser, serverPublicIP, keyPath, cmd)
		}

		// Server setup — differs between Ubuntu and AL2023.
		if useAL2023 {
			_, _ = serverSSH("echo 0 | sudo tee /proc/sys/kernel/io_uring_disabled > /dev/null 2>&1")
		} else {
			_, _ = serverSSH("echo 0 | sudo tee /proc/sys/kernel/apparmor_restrict_unprivileged_io_uring > /dev/null 2>&1")
		}
		_, _ = serverSSH("mkdir -p /tmp/analysis")
		if err := awsSCPAs(sshUser, serverBin, "/tmp/analysis/server", serverPublicIP, keyPath); err != nil {
			return err
		}
		_, _ = serverSSH("chmod +x /tmp/analysis/server")

		// Install sysstat for pidstat, tune kernel for high connection counts.
		var installSysstat string
		if useAL2023 {
			installSysstat = "sudo dnf install -y -q sysstat"
		} else {
			installSysstat = "sudo apt-get update -qq && sudo apt-get install -y -qq sysstat"
		}
		_, _ = serverSSH(strings.Join([]string{
			installSysstat,
			"sudo sysctl -w net.core.somaxconn=131072",
			"sudo sysctl -w net.ipv4.ip_local_port_range='1024 65535'",
			"sudo sysctl -w net.ipv4.tcp_tw_reuse=1",
			"sudo sysctl -w net.ipv4.tcp_fin_timeout=10",
			"sudo sysctl -w fs.file-max=200000",
			"sudo sysctl -w fs.nr_open=200000",
			"sudo bash -c 'echo \"* soft nofile 200000\" >> /etc/security/limits.conf'",
			"sudo bash -c 'echo \"* hard nofile 200000\" >> /etc/security/limits.conf'",
			"sudo bash -c 'echo \"root soft nofile 200000\" >> /etc/security/limits.conf'",
			"sudo bash -c 'echo \"root hard nofile 200000\" >> /etc/security/limits.conf'",
			"sudo bash -c 'echo DefaultLimitNOFILE=200000 >> /etc/systemd/system.conf'",
		}, " && "))

		kernelOut, _ := serverSSH("uname -r")
		fmt.Printf("  Kernel: %s", kernelOut)
		cpuOut, _ := serverSSH("nproc")
		fmt.Printf("  CPUs: %s", cpuOut)
		// Check NIC scatter-gather support (needed for true SEND_ZC zero-copy).
		sgOut, _ := serverSSH("ethtool -k $(ip -o link show | awk -F': ' '/state UP/{print $2}' | head -1) 2>/dev/null | grep scatter")
		if sgOut != "" {
			fmt.Printf("  NIC: %s", strings.TrimSpace(sgOut))
			fmt.Println()
		}

		// Launch client (always Ubuntu — loadgen doesn't need special kernel).
		clientID, clientPublicIP, err := awsLaunchInstance(clientAMI, instanceType, keyName, sgID, arch)
		if err != nil {
			return err
		}
		allInstanceIDs = append(allInstanceIDs, clientID)
		if err := awsWaitSSH(clientPublicIP, keyPath); err != nil {
			return err
		}
		fmt.Printf("  Client: %s\n", clientPublicIP)

		// Upload loadgen + install wrk (wrk needed for Connection: close H1).
		_, _ = awsSSH(clientPublicIP, keyPath, "mkdir -p /tmp/analysis")
		if err := awsSCP(loadgenBin, "/tmp/analysis/loadgen", clientPublicIP, keyPath); err != nil {
			return fmt.Errorf("upload loadgen: %w", err)
		}
		_, _ = awsSSH(clientPublicIP, keyPath, "chmod +x /tmp/analysis/loadgen")
		if _, err := awsSSH(clientPublicIP, keyPath, "sudo apt-get update -qq && sudo apt-get install -y -qq wrk"); err != nil {
			return fmt.Errorf("install wrk: %w", err)
		}
		// Tune client for 65K+ connections: fd limits, port range, TCP recycling.
		_, _ = awsSSH(clientPublicIP, keyPath, strings.Join([]string{
			"sudo sysctl -w net.ipv4.ip_local_port_range='1024 65535'",
			"sudo sysctl -w net.ipv4.tcp_tw_reuse=1",
			"sudo sysctl -w net.ipv4.tcp_fin_timeout=10",
			"sudo sysctl -w net.core.somaxconn=131072",
			"sudo sysctl -w fs.file-max=200000",
			"sudo sysctl -w fs.nr_open=200000",
			"sudo bash -c 'echo \"* soft nofile 200000\" >> /etc/security/limits.conf'",
			"sudo bash -c 'echo \"* hard nofile 200000\" >> /etc/security/limits.conf'",
			"sudo bash -c 'echo \"root soft nofile 200000\" >> /etc/security/limits.conf'",
			"sudo bash -c 'echo \"root hard nofile 200000\" >> /etc/security/limits.conf'",
			"sudo bash -c 'echo DefaultLimitNOFILE=200000 >> /etc/systemd/system.conf'",
		}, " && "))

		// Collect results across multiple runs for median computation.
		// allRuns[runIdx] holds one full sweep; we compute median RPS per config.
		allRuns := make([][]analysisResult, numRuns)
		globalRun := 0

		for runIdx := range numRuns {
			if numRuns > 1 {
				fmt.Printf("\n--- Run %d/%d ---\n", runIdx+1, numRuns)
			}
			var runResults []analysisResult

			for _, wl := range workloads {
				for _, proto := range protocols {
					for _, conns := range connCounts {
						for _, eng := range engines {
							globalRun++
							fmt.Printf("[%d/%d] %s %s %s %dc: ",
								globalRun, totalPerArch, wl.name, eng, proto, conns)

						result := runAnalysisBenchmark(
							serverPublicIP, serverPrivateIP, clientPublicIP,
							keyPath, sshUser, eng, proto, wl, conns, duration,
						)
						result.arch = arch
						runResults = append(runResults, result)

						fmt.Printf("%.0f rps", result.rps)
						if result.cpuPct > 0 {
							fmt.Printf(", cpu=%.0f%%", result.cpuPct)
						}
						if result.rssMB > 0 {
							fmt.Printf(", rss=%.1fMB", result.rssMB)
						}
						fmt.Println()
					}
				}
			}
		}
			allRuns[runIdx] = runResults
		}

		// Compute median results across runs.
		medianResults := computeMedianResults(allRuns)

		report := generateAnalysisReport(arch, instanceType, kernelOut, cpuOut, duration, workloads, protocols, connCounts, engines, medianResults)
		fmt.Println("\n" + report)

		reportPath := filepath.Join(dir, fmt.Sprintf("report-%s.txt", arch))
		_ = os.WriteFile(reportPath, []byte(report), 0o644)
		fmt.Printf("Report saved to: %s\n", reportPath)
	}

	return nil
}

// analysisWorkload defines a workload pattern.
type analysisWorkload struct {
	name      string
	path      string
	connClose bool
}

func workloadNames(wls []analysisWorkload) []string {
	names := make([]string, len(wls))
	for i, w := range wls {
		names[i] = w.name
	}
	return names
}

// analysisResult holds all metrics for one benchmark run.
type analysisResult struct {
	arch, workload, proto, engine string
	conns                         int
	rps                           float64
	// H1 latency percentiles (microseconds).
	latP50, latP75, latP90, latP99 float64
	// H2 latency stats (microseconds).
	latMin, latMax, latMean, latSD float64
	cpuPct                         float64
	rssMB                          float64
}

func analysisInstanceType(arch string) string {
	if arch == "arm64" {
		return "c7g.16xlarge" // 64 vCPU Graviton3
	}
	return "c7i.16xlarge" // 64 vCPU Intel
}

// runAnalysisBenchmark executes a single benchmark run and collects all metrics.
func runAnalysisBenchmark(
	serverPublicIP, serverPrivateIP, clientPublicIP, keyPath, sshUser,
	eng, proto string, wl analysisWorkload, conns int, duration string,
) analysisResult {
	serverSSH := func(cmd string) (string, error) {
		return awsSSHAs(sshUser, serverPublicIP, keyPath, cmd)
	}
	result := analysisResult{
		workload: wl.name,
		proto:    proto,
		engine:   eng,
		conns:    conns,
	}

	// Kill any leftover server and clean up metrics files.
	_, _ = serverSSH(
		"sudo pkill -9 -f 'analysis/server' 2>/dev/null; sudo pkill pidstat 2>/dev/null; "+
			"rm -f /tmp/analysis/pidstat.log /tmp/analysis/server.pid; sleep 1")

	// Map protocol name to server env value.
	serverProto := proto
	if proto == "h2" {
		serverProto = "h2"
	}

	// Start server and record its PID. Use prlimit to set both memlock
	// (for io_uring mmap) and nofile (for 65K+ connections) on the process.
	startCmd := fmt.Sprintf(
		"sudo bash -c 'prlimit --memlock=unlimited --nofile=200000 env ENGINE=%s PROTOCOL=%s PORT=18080 /tmp/analysis/server > /tmp/analysis/server.log 2>&1 & echo $! > /tmp/analysis/server.pid'",
		eng, serverProto)
	_, _ = serverSSH(startCmd)
	time.Sleep(3 * time.Second)

	// Print server startup log (shows probe results for io_uring).
	if eng == "iouring" {
		serverLog, _ := serverSSH("head -5 /tmp/analysis/server.log 2>/dev/null")
		if serverLog != "" {
			fmt.Print("  ", strings.ReplaceAll(strings.TrimSpace(serverLog), "\n", "\n  "), "\n")
		}
	}

	// Read the recorded PID.
	pidOut, _ := serverSSH("cat /tmp/analysis/server.pid 2>/dev/null")
	pid := strings.TrimSpace(pidOut)

	// Start pidstat in background via a wrapper script to survive SSH disconnect.
	if pid != "" {
		_, _ = serverSSH(fmt.Sprintf(
			"sudo bash -c 'nohup pidstat -u 1 -p %s > /tmp/analysis/pidstat.log 2>&1 &'", pid))
	}

	// Build load command. Use loadgen for keep-alive (H1+H2) and all H2.
	// Use wrk for Connection: close H1 (loadgen's Go net.Dial reconnect
	// doesn't fail fast enough against io_uring servers).
	var loadCmd string
	useWrk := wl.connClose && proto == "h1"

	if useWrk {
		loadCmd = fmt.Sprintf("sudo prlimit --nofile=200000 wrk -t4 -c%d -d%ss --latency -H \"Connection: close\" http://%s:18080%s",
			conns, duration, serverPrivateIP, wl.path)
	} else {
		loadArgs := fmt.Sprintf("-url http://%s:18080%s -duration %ss -connections %d",
			serverPrivateIP, wl.path, duration, conns)
		if proto == "h2" {
			loadArgs += fmt.Sprintf(" -h2 -h2-conns %d -h2-streams 100", min(conns, 256))
		}
		if wl.connClose {
			loadArgs += " -close"
		}
		loadCmd = fmt.Sprintf("sudo prlimit --nofile=200000 /tmp/analysis/loadgen %s", loadArgs)
	}

	loadOut, _ := awsSSH(clientPublicIP, keyPath, loadCmd)
	if useWrk {
		if m := wrkRPSRegex.FindStringSubmatch(loadOut); m != nil {
			result.rps, _ = strconv.ParseFloat(m[1], 64)
		}
		result.latP50, result.latP75, result.latP90, result.latP99 = parseWrkLatency(loadOut)
	} else {
		parseLoadgenResult(loadOut, &result)
	}

	// Collect RSS from /proc BEFORE killing the server.
	if pid != "" {
		procOut, _ := serverSSH(
			fmt.Sprintf("sudo cat /proc/%s/status 2>/dev/null | grep VmHWM", pid))
		result.rssMB = parseProcRSS(procOut)
	}

	// Collect CPU from pidstat.
	if pid != "" {
		_, _ = serverSSH("sudo pkill pidstat 2>/dev/null; sleep 1")
		pidstatOut, _ := serverSSH("cat /tmp/analysis/pidstat.log 2>/dev/null")
		result.cpuPct = parsePidstatCPU(pidstatOut)
	}

	// Kill server.
	_, _ = serverSSH("sudo pkill -9 -f 'analysis/server' 2>/dev/null")
	time.Sleep(2 * time.Second)

	return result
}

// --- Parsing functions ---

// parseWrkLatency extracts p50/p75/p90/p99 from wrk --latency output.
// wrk prints lines like "  50%  245.00us" or "  99%   12.34ms".
var wrkLatencyRegex = regexp.MustCompile(`^\s+(\d+)%\s+([\d.]+)(us|ms|s)\s*$`)

func parseWrkLatency(out string) (p50, p75, p90, p99 float64) {
	for _, line := range strings.Split(out, "\n") {
		m := wrkLatencyRegex.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pct, _ := strconv.Atoi(m[1])
		val, _ := strconv.ParseFloat(m[2], 64)
		us := toMicroseconds(val, m[3])
		switch pct {
		case 50:
			p50 = us
		case 75:
			p75 = us
		case 90:
			p90 = us
		case 99:
			p99 = us
		}
	}
	return
}

// parseH2loadLatency extracts min/max/mean/sd from h2load output.
// h2load prints: "time for request:    1.23ms    45.67ms     5.89ms     8.12ms"
var h2loadTimeRegex = regexp.MustCompile(`time for request:\s+([\d.]+)(us|ms|s)\s+([\d.]+)(us|ms|s)\s+([\d.]+)(us|ms|s)\s+([\d.]+)(us|ms|s)`)

func parseH2loadLatency(out string) (minUs, maxUs, meanUs, sdUs float64) {
	m := h2loadTimeRegex.FindStringSubmatch(out)
	if m == nil {
		return
	}
	v1, _ := strconv.ParseFloat(m[1], 64)
	v2, _ := strconv.ParseFloat(m[3], 64)
	v3, _ := strconv.ParseFloat(m[5], 64)
	v4, _ := strconv.ParseFloat(m[7], 64)
	minUs = toMicroseconds(v1, m[2])
	maxUs = toMicroseconds(v2, m[4])
	meanUs = toMicroseconds(v3, m[6])
	sdUs = toMicroseconds(v4, m[8])
	return
}

// parsePidstatCPU averages the %CPU column from pidstat output.
// pidstat output lines look like:
// HH:MM:SS   UID   PID  %usr  %system  %guest  %wait  %CPU  CPU  Command
var pidstatCPURegex = regexp.MustCompile(`\d+:\d+:\d+\s+\d+\s+\d+\s+[\d.]+\s+[\d.]+\s+[\d.]+\s+[\d.]+\s+([\d.]+)`)

func parsePidstatCPU(out string) float64 {
	var sum float64
	var n int
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Average") {
			continue
		}
		m := pidstatCPURegex.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v, _ := strconv.ParseFloat(m[1], 64)
		if v > 0 {
			sum += v
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// parseProcRSS extracts VmHWM (peak RSS) from /proc/PID/status.
// Line format: "VmHWM:    45678 kB"
var vmHWMRegex = regexp.MustCompile(`VmHWM:\s+(\d+)\s+kB`)

func parseProcRSS(out string) float64 {
	m := vmHWMRegex.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	kb, _ := strconv.ParseFloat(m[1], 64)
	return kb / 1024.0
}

// toMicroseconds converts a value with unit to microseconds.
func toMicroseconds(val float64, unit string) float64 {
	switch unit {
	case "us":
		return val
	case "ms":
		return val * 1000
	case "s":
		return val * 1_000_000
	}
	return val
}

// loadgenOutput matches the JSON output from the custom loadgen binary.
type loadgenOutput struct {
	Requests       int64   `json:"requests"`
	Errors         int64   `json:"errors"`
	RequestsPerSec float64 `json:"requests_per_sec"`
	Latency        struct {
		Avg   jsonDuration `json:"avg"`
		Min   jsonDuration `json:"min"`
		Max   jsonDuration `json:"max"`
		P50   jsonDuration `json:"p50"`
		P75   jsonDuration `json:"p75"`
		P90   jsonDuration `json:"p90"`
		P99   jsonDuration `json:"p99"`
		P999  jsonDuration `json:"p99_9"`
		P9999 jsonDuration `json:"p99_99"`
	} `json:"latency"`
	ClientCPUPercent float64 `json:"client_cpu_percent"`
}

// jsonDuration is a time.Duration that supports JSON nanosecond integers.
type jsonDuration time.Duration

func (d *jsonDuration) UnmarshalJSON(b []byte) error {
	var ns int64
	if err := json.Unmarshal(b, &ns); err != nil {
		return err
	}
	*d = jsonDuration(time.Duration(ns))
	return nil
}

func (d jsonDuration) us() float64 {
	return float64(time.Duration(d)) / float64(time.Microsecond)
}

// parseLoadgenResult parses the JSON output from loadgen into an analysisResult.
// Provides unified latency percentiles for both H1 and H2 protocols.
func parseLoadgenResult(out string, r *analysisResult) {
	// Find the JSON object in the output (skip any log lines before it).
	start := strings.Index(out, "{")
	if start < 0 {
		return
	}
	jsonStr := out[start:]

	var lg loadgenOutput
	if err := json.Unmarshal([]byte(jsonStr), &lg); err != nil {
		return
	}

	r.rps = lg.RequestsPerSec
	r.latP50 = lg.Latency.P50.us()
	r.latP75 = lg.Latency.P75.us()
	r.latP90 = lg.Latency.P90.us()
	r.latP99 = lg.Latency.P99.us()
	// Also populate H2-style fields for unified reporting.
	r.latMean = lg.Latency.Avg.us()
	r.latMax = lg.Latency.Max.us()
	r.latMin = lg.Latency.Min.us()
}

// computeMedianResults takes N full sweeps and returns one result per config
// with median RPS and p99. For a single run, returns the results as-is.
func computeMedianResults(allRuns [][]analysisResult) []analysisResult {
	if len(allRuns) == 1 {
		return allRuns[0]
	}

	// Group results by config key.
	type key struct {
		workload, proto, engine string
		conns                   int
	}
	grouped := make(map[key][]analysisResult)
	for _, run := range allRuns {
		for _, r := range run {
			k := key{r.workload, r.proto, r.engine, r.conns}
			grouped[k] = append(grouped[k], r)
		}
	}

	// Compute median for each config.
	var results []analysisResult
	for _, run := range allRuns[0] {
		k := key{run.workload, run.proto, run.engine, run.conns}
		runs := grouped[k]
		if len(runs) == 0 {
			results = append(results, run)
			continue
		}

		// Sort by RPS and take median.
		rpsValues := make([]float64, len(runs))
		for i, r := range runs {
			rpsValues[i] = r.rps
		}
		sort.Float64s(rpsValues)
		medIdx := len(rpsValues) / 2

		// Find the run closest to median RPS and use its full result
		// (preserves latency/CPU/RSS correlation).
		medRPS := rpsValues[medIdx]
		best := runs[0]
		bestDist := math.Abs(best.rps - medRPS)
		for _, r := range runs[1:] {
			d := math.Abs(r.rps - medRPS)
			if d < bestDist {
				best = r
				bestDist = d
			}
		}
		results = append(results, best)
	}
	return results
}

// --- Report generation ---

func generateAnalysisReport(
	arch, instance, kernel, cpus, duration string,
	workloads []analysisWorkload, protocols []string, connCounts []int,
	engines []string, results []analysisResult,
) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("io_uring vs epoll Engine Analysis — %s\n", arch))
	sb.WriteString(fmt.Sprintf("Instance: %s, Kernel: %s, CPUs: %s",
		instance, strings.TrimSpace(kernel), strings.TrimSpace(cpus)))
	sb.WriteString(fmt.Sprintf("Duration: %ss per run\n\n", duration))

	// Build lookup: workload|proto|engine|conns → result.
	lookup := make(map[string]analysisResult)
	for _, r := range results {
		key := fmt.Sprintf("%s|%s|%s|%d", r.workload, r.proto, r.engine, r.conns)
		lookup[key] = r
	}

	type advantage struct {
		workload, proto string
		conns           int
		pct             float64
	}
	var allAdvantages []advantage

	for _, wl := range workloads {
		sb.WriteString(fmt.Sprintf("=== %s", strings.ToUpper(wl.name)))
		if wl.connClose {
			sb.WriteString(" (Connection: close)")
		} else {
			sb.WriteString(" (keep-alive)")
		}
		if wl.path == "/download" {
			sb.WriteString(" — 64KB response")
		}
		sb.WriteString(fmt.Sprintf(" — %s %s ===\n\n", arch, instance))

		for _, proto := range protocols {
			protoLabel := strings.ToUpper(proto)
			sb.WriteString(fmt.Sprintf("--- %s ---\n", protoLabel))

			sb.WriteString(fmt.Sprintf("%-7s | %-8s | %10s | %9s | %9s | %9s | %9s | %5s | %8s\n",
				"Conns", "Engine", "RPS", "p50(us)", "p75(us)", "p90(us)", "p99(us)", "CPU%", "RSS(MB)"))
			sb.WriteString(strings.Repeat("-", 96) + "\n")

			for _, conns := range connCounts {
				for _, eng := range engines {
					key := fmt.Sprintf("%s|%s|%s|%d", wl.name, proto, eng, conns)
					r := lookup[key]

					sb.WriteString(fmt.Sprintf("%-7d | %-8s | %10.0f | %9s | %9s | %9s | %9s | %5s | %8s\n",
						conns, eng, r.rps,
						fmtLatency(r.latP50), fmtLatency(r.latP75),
						fmtLatency(r.latP90), fmtLatency(r.latP99),
						fmtCPU(r.cpuPct), fmtRSS(r.rssMB)))
				}

				// Delta row.
				epoKey := fmt.Sprintf("%s|%s|epoll|%d", wl.name, proto, conns)
				iouKey := fmt.Sprintf("%s|%s|iouring|%d", wl.name, proto, conns)
				epo := lookup[epoKey]
				iou := lookup[iouKey]

				rpsDelta := "N/A"
				if epo.rps > 0 {
					pct := (iou.rps - epo.rps) / epo.rps * 100
					rpsDelta = fmt.Sprintf("%+.1f%%", pct)
					allAdvantages = append(allAdvantages, advantage{wl.name, proto, conns, pct})
				}

				sb.WriteString(fmt.Sprintf("%-7d | %-8s | %10s | %9s | %9s | %9s | %9s | %5s | %8s\n",
					conns, "delta", rpsDelta,
						fmtDeltaPct(epo.latP50, iou.latP50),
						fmtDeltaPct(epo.latP75, iou.latP75),
						fmtDeltaPct(epo.latP90, iou.latP90),
						fmtDeltaPct(epo.latP99, iou.latP99),
						fmtDeltaAbs(epo.cpuPct, iou.cpuPct),
						fmtDeltaPct(epo.rssMB, iou.rssMB)))
			}
			sb.WriteString("\n")
		}
	}

	// Sort advantages descending.
	for i := range allAdvantages {
		for j := i + 1; j < len(allAdvantages); j++ {
			if allAdvantages[j].pct > allAdvantages[i].pct {
				allAdvantages[i], allAdvantages[j] = allAdvantages[j], allAdvantages[i]
			}
		}
	}

	// Best workloads for io_uring.
	sb.WriteString("=== SUMMARY: io_uring best advantages ===\n")
	for i, a := range allAdvantages {
		if i >= 10 {
			break
		}
		sb.WriteString(fmt.Sprintf("  %s %s %dc: %+.1f%%\n", a.workload, a.proto, a.conns, a.pct))
	}

	sb.WriteString("\n=== SUMMARY: epoll best advantages ===\n")
	for i := len(allAdvantages) - 1; i >= 0 && i >= len(allAdvantages)-10; i-- {
		a := allAdvantages[i]
		if a.pct >= 0 {
			break
		}
		sb.WriteString(fmt.Sprintf("  %s %s %dc: %+.1f%%\n", a.workload, a.proto, a.conns, a.pct))
	}

	// Per-workload summary.
	sb.WriteString("\n=== SUMMARY: per-workload average io_uring advantage ===\n")
	for _, wl := range workloads {
		var sum float64
		var n int
		for _, a := range allAdvantages {
			if a.workload == wl.name {
				sum += a.pct
				n++
			}
		}
		if n > 0 {
			sb.WriteString(fmt.Sprintf("  %-22s: %+.1f%% (%d configs)\n", wl.name, sum/float64(n), n))
		}
	}

	// Crossover analysis: find the connection count where io_uring flips from
	// negative to positive advantage (or vice versa) for each workload×protocol.
	sb.WriteString("\n=== SUMMARY: crossover connection counts ===\n")
	for _, wl := range workloads {
		for _, proto := range protocols {
			var lastPct float64
			crossover := "none"
			for i, conns := range connCounts {
				epoKey := fmt.Sprintf("%s|%s|epoll|%d", wl.name, proto, conns)
				iouKey := fmt.Sprintf("%s|%s|iouring|%d", wl.name, proto, conns)
				epo := lookup[epoKey]
				iou := lookup[iouKey]
				if epo.rps <= 0 {
					continue
				}
				pct := (iou.rps - epo.rps) / epo.rps * 100
				if i > 0 && lastPct < 0 && pct >= 0 {
					crossover = fmt.Sprintf("~%d", conns)
				} else if i > 0 && lastPct >= 0 && pct < 0 {
					crossover = fmt.Sprintf("~%d (reverses)", conns)
				}
				lastPct = pct
			}
			sb.WriteString(fmt.Sprintf("  %s %s: %s\n", wl.name, proto, crossover))
		}
	}

	// Overall recommendation.
	sb.WriteString("\n=== RECOMMENDATION ===\n")
	var totalAdv float64
	for _, a := range allAdvantages {
		totalAdv += a.pct
	}
	n := len(allAdvantages)
	if n > 0 {
		avg := totalAdv / float64(n)
		if avg > 5 {
			sb.WriteString(fmt.Sprintf("  io_uring shows clear advantage (avg %+.1f%%). Adaptive engine should prefer io_uring.\n", avg))
		} else if avg > 0 {
			sb.WriteString(fmt.Sprintf("  io_uring shows slight advantage (avg %+.1f%%). Workload-dependent selection recommended.\n", avg))
		} else if avg > -5 {
			sb.WriteString(fmt.Sprintf("  Engines are comparable (avg %+.1f%%). Connection count is the key differentiator.\n", avg))
		} else {
			sb.WriteString(fmt.Sprintf("  epoll shows clear advantage (avg %+.1f%%). Investigate io_uring configuration.\n", avg))
		}
	}

	// Find best workload for io_uring.
	bestWl := ""
	bestWlAvg := math.Inf(-1)
	for _, wl := range workloads {
		var sum float64
		var wn int
		for _, a := range allAdvantages {
			if a.workload == wl.name {
				sum += a.pct
				wn++
			}
		}
		if wn > 0 {
			avg := sum / float64(wn)
			if avg > bestWlAvg {
				bestWlAvg = avg
				bestWl = wl.name
			}
		}
	}
	if bestWl != "" {
		sb.WriteString(fmt.Sprintf("  Best workload for io_uring: %s (avg %+.1f%%)\n", bestWl, bestWlAvg))
	}

	return sb.String()
}

// --- Formatting helpers ---

func fmtLatency(us float64) string {
	if us <= 0 {
		return "N/A"
	}
	if us >= 1_000_000 {
		return fmt.Sprintf("%.1fs", us/1_000_000)
	}
	if us >= 1000 {
		return fmt.Sprintf("%.1fms", us/1000)
	}
	return fmt.Sprintf("%.0fus", us)
}

func fmtCPU(pct float64) string {
	if pct <= 0 {
		return "N/A"
	}
	return fmt.Sprintf("%.0f", pct)
}

func fmtRSS(mb float64) string {
	if mb <= 0 {
		return "N/A"
	}
	return fmt.Sprintf("%.1f", mb)
}

func fmtDeltaPct(base, current float64) string {
	if base <= 0 || current <= 0 {
		return "N/A"
	}
	pct := (current - base) / base * 100
	return fmt.Sprintf("%+.1f%%", pct)
}

func fmtDeltaAbs(base, current float64) string {
	if base <= 0 && current <= 0 {
		return "N/A"
	}
	return fmt.Sprintf("%+.1f", current-base)
}
