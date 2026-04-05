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
	"sort"
	"strconv"
	"strings"
	"time"
)

// middlewareNames lists the middleware tested individually.
var middlewareNames = []string{
	"Logger", "Recovery", "CORS_Preflight", "CORS_Simple", "RateLimit",
	"RequestID", "Timeout", "BodyLimit", "Secure", "KeyAuth",
	"CSRF", "BasicAuth",
}

// frameworkNames lists the frameworks compared.
var frameworkNames = []string{"Celeris", "Fiber", "Echo", "Chi", "Stdlib"}

// chainNames lists the chain benchmarks.
var chainNames = []string{"ChainAPI", "ChainAuth", "ChainSecurity", "ChainFullStack"}

// benchResult holds a parsed benchmark result.
type benchResult struct {
	Name     string
	NsPerOp  float64
	AllocsOp int64
	BytesOp  int64
}

// parseBenchResults parses `go test -bench` output into structured results.
func parseBenchResults(output string) []benchResult {
	re := regexp.MustCompile(`^Benchmark(\S+)\s+\d+\s+([\d.]+)\s+ns/op\s+(\d+)\s+B/op\s+(\d+)\s+allocs/op`)
	var results []benchResult
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		m := re.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		nsOp, _ := strconv.ParseFloat(m[2], 64)
		bytesOp, _ := strconv.ParseInt(m[3], 10, 64)
		allocsOp, _ := strconv.ParseInt(m[4], 10, 64)
		results = append(results, benchResult{
			Name:     m[1],
			NsPerOp:  nsOp,
			AllocsOp: allocsOp,
			BytesOp:  bytesOp,
		})
	}
	return results
}

// MiddlewareBenchmark runs the full middleware comparison suite (individual + chains)
// across Celeris, Fiber, Echo, Chi, and stdlib.
func MiddlewareBenchmark() error {
	count := "6"
	if c := os.Getenv("BENCH_COUNT"); c != "" {
		count = c
	}
	benchtime := "1s"
	if t := os.Getenv("BENCH_TIME"); t != "" {
		benchtime = t
	}

	dir, err := resultsDir("middleware-benchmark")
	if err != nil {
		return err
	}

	fmt.Printf("Middleware Benchmark Suite\n")
	fmt.Printf("Count: %s, BenchTime: %s\n", count, benchtime)
	fmt.Printf("Results: %s\n\n", dir)

	// Run benchmarks from the benchcmp module directory.
	fmt.Println("Running benchmarks...")
	benchcmpDir := filepath.Join(repoRoot(), "test", "benchcmp")
	cmd := exec.Command("go", "test",
		"-bench=.", "-benchmem", "-run=^$",
		"-count="+count, "-benchtime="+benchtime,
		"./...",
	)
	cmd.Dir = benchcmpDir
	out, err := cmd.CombinedOutput()
	rawPath := filepath.Join(dir, "raw.txt")
	_ = os.WriteFile(rawPath, out, 0o644)
	if err != nil {
		fmt.Printf("Benchmark run failed: %v\n", err)
		fmt.Println(string(out))
		return err
	}
	fmt.Printf("Raw output saved to: %s\n\n", rawPath)

	results := parseBenchResults(string(out))
	if len(results) == 0 {
		fmt.Println("No benchmark results parsed.")
		return nil
	}

	// Aggregate by name (median of multiple runs).
	medians := aggregateMedians(results)

	// Generate reports.
	var report strings.Builder
	report.WriteString(generateMiddlewareHeader(count, benchtime))
	report.WriteString(generateIndividualReport(medians))
	report.WriteString(generateChainReport(medians))
	report.WriteString(generateSummary(medians))

	reportText := report.String()
	fmt.Print(reportText)

	reportPath := filepath.Join(dir, "report.txt")
	_ = os.WriteFile(reportPath, []byte(reportText), 0o644)
	fmt.Printf("\nReport saved to: %s\n", reportPath)
	return nil
}

// MiddlewareProfile runs benchmarks with CPU profiling for each middleware
// and generates pprof profiles.
func MiddlewareProfile() error {
	dir, err := resultsDir("middleware-profile")
	if err != nil {
		return err
	}

	fmt.Printf("Middleware Profiling Suite\n")
	fmt.Printf("Results: %s\n\n", dir)

	// Profile individual middleware.
	for _, mw := range middlewareNames {
		pattern := fmt.Sprintf("^Benchmark%s_Celeris$", mw)
		profPath := filepath.Join(dir, fmt.Sprintf("%s.cpu.prof", strings.ToLower(mw)))
		fmt.Printf("  Profiling %s...\n", mw)

		cmd := exec.Command("go", "test",
			"-bench="+pattern, "-benchmem", "-run=^$",
			"-benchtime=3s", "-count=1",
			"-cpuprofile="+profPath,
			"./...",
		)
		cmd.Dir = filepath.Join(repoRoot(), "test", "benchcmp")
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("    WARNING: %s profiling failed: %v\n", mw, err)
			continue
		}

		// Save raw output.
		rawPath := filepath.Join(dir, fmt.Sprintf("%s.txt", strings.ToLower(mw)))
		_ = os.WriteFile(rawPath, out, 0o644)

		// Generate top-20 text.
		if isValidProfile(profPath) {
			topOut, err := output("go", "tool", "pprof", "-top", "-cum", "-nodecount=20", profPath)
			if err == nil {
				topPath := filepath.Join(dir, fmt.Sprintf("%s.top20.txt", strings.ToLower(mw)))
				_ = os.WriteFile(topPath, []byte(topOut), 0o644)
			}
		}
	}

	// Profile chains.
	for _, chain := range chainNames {
		pattern := fmt.Sprintf("^Benchmark%s_Celeris$", chain)
		profPath := filepath.Join(dir, fmt.Sprintf("%s.cpu.prof", strings.ToLower(chain)))
		fmt.Printf("  Profiling %s...\n", chain)

		cmd := exec.Command("go", "test",
			"-bench="+pattern, "-benchmem", "-run=^$",
			"-benchtime=3s", "-count=1",
			"-cpuprofile="+profPath,
			"./...",
		)
		cmd.Dir = filepath.Join(repoRoot(), "test", "benchcmp")
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("    WARNING: %s profiling failed: %v\n", chain, err)
			continue
		}

		rawPath := filepath.Join(dir, fmt.Sprintf("%s.txt", strings.ToLower(chain)))
		_ = os.WriteFile(rawPath, out, 0o644)

		if isValidProfile(profPath) {
			topOut, err := output("go", "tool", "pprof", "-top", "-cum", "-nodecount=20", profPath)
			if err == nil {
				topPath := filepath.Join(dir, fmt.Sprintf("%s.top20.txt", strings.ToLower(chain)))
				_ = os.WriteFile(topPath, []byte(topOut), 0o644)
			}
			// Generate flamegraph SVG.
			svgPath := filepath.Join(dir, fmt.Sprintf("%s.flamegraph.svg", strings.ToLower(chain)))
			_ = exec.Command("go", "tool", "pprof", "-svg", "-output="+svgPath, profPath).Run()
		}
	}

	fmt.Printf("\nProfiles saved to: %s\n", dir)
	return nil
}

// repoRoot returns the absolute path to the repository root.
func repoRoot() string {
	cwd, _ := os.Getwd()
	return cwd
}

// aggregateMedians computes the median ns/op, allocs, and bytes for each benchmark name.
func aggregateMedians(results []benchResult) map[string]benchResult {
	byName := make(map[string][]benchResult)
	for _, r := range results {
		byName[r.Name] = append(byName[r.Name], r)
	}
	medians := make(map[string]benchResult)
	for name, runs := range byName {
		nsVals := make([]float64, len(runs))
		for i, r := range runs {
			nsVals[i] = r.NsPerOp
		}
		sort.Float64s(nsVals)
		n := len(nsVals)
		var medNs float64
		if n%2 == 0 {
			medNs = (nsVals[n/2-1] + nsVals[n/2]) / 2
		} else {
			medNs = nsVals[n/2]
		}
		medians[name] = benchResult{
			Name:     name,
			NsPerOp:  medNs,
			AllocsOp: runs[0].AllocsOp,
			BytesOp:  runs[0].BytesOp,
		}
	}
	return medians
}

func generateMiddlewareHeader(count, benchtime string) string {
	var sb strings.Builder
	sb.WriteString("Celeris Middleware Benchmark Report\n")
	sb.WriteString(fmt.Sprintf("Date: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("Count: %s, BenchTime: %s\n", count, benchtime))
	sb.WriteString("Frameworks: Celeris, Fiber v3, Echo v4, Chi v5, net/http stdlib\n\n")
	return sb.String()
}

func generateIndividualReport(medians map[string]benchResult) string {
	var sb strings.Builder
	sb.WriteString("=== Individual Middleware Comparison ===\n\n")

	for _, mw := range middlewareNames {
		sb.WriteString(fmt.Sprintf("--- %s ---\n", mw))
		sb.WriteString(fmt.Sprintf("%-12s | %12s | %8s | %8s | %10s\n",
			"Framework", "ns/op", "B/op", "allocs", "vs Celeris"))
		sb.WriteString(strings.Repeat("-", 60) + "\n")

		celerisKey := mw + "_Celeris"
		if strings.HasSuffix(mw, "_Preflight") || strings.HasSuffix(mw, "_Simple") {
			celerisKey = mw + "_Celeris"
		}
		celerisNs := medians[celerisKey].NsPerOp

		for _, fw := range frameworkNames {
			key := mw + "_" + fw
			r, ok := medians[key]
			if !ok {
				continue
			}
			delta := ""
			if celerisNs > 0 && fw != "Celeris" {
				pct := (r.NsPerOp - celerisNs) / celerisNs * 100
				delta = fmt.Sprintf("%+.0f%%", pct)
			}
			sb.WriteString(fmt.Sprintf("%-12s | %12.1f | %8d | %8d | %10s\n",
				fw, r.NsPerOp, r.BytesOp, r.AllocsOp, delta))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func generateChainReport(medians map[string]benchResult) string {
	var sb strings.Builder
	sb.WriteString("=== Middleware Chain Comparison ===\n\n")

	chainDescs := map[string]string{
		"ChainAPI":       "Public API: Recovery + RequestID + CORS + RateLimit",
		"ChainAuth":      "Auth API: Recovery + Logger + RequestID + BasicAuth",
		"ChainSecurity":  "Security: Recovery + Secure + CSRF + KeyAuth",
		"ChainFullStack": "Full Stack: Recovery + Logger + RequestID + CORS + RateLimit + Secure + Timeout",
	}

	for _, chain := range chainNames {
		desc := chainDescs[chain]
		sb.WriteString(fmt.Sprintf("--- %s ---\n", desc))
		sb.WriteString(fmt.Sprintf("%-12s | %12s | %8s | %8s | %10s\n",
			"Framework", "ns/op", "B/op", "allocs", "vs Celeris"))
		sb.WriteString(strings.Repeat("-", 60) + "\n")

		celerisNs := medians[chain+"_Celeris"].NsPerOp

		for _, fw := range frameworkNames {
			key := chain + "_" + fw
			r, ok := medians[key]
			if !ok {
				continue
			}
			delta := ""
			if celerisNs > 0 && fw != "Celeris" {
				pct := (r.NsPerOp - celerisNs) / celerisNs * 100
				delta = fmt.Sprintf("%+.0f%%", pct)
			}
			sb.WriteString(fmt.Sprintf("%-12s | %12.1f | %8d | %8d | %10s\n",
				fw, r.NsPerOp, r.BytesOp, r.AllocsOp, delta))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func generateSummary(medians map[string]benchResult) string {
	var sb strings.Builder
	sb.WriteString("=== Summary: Celeris vs Competitors (median ns/op) ===\n\n")

	type fwSummary struct {
		name     string
		wins     int
		losses   int
		total    int
		avgDelta float64
	}

	summaries := make(map[string]*fwSummary)
	for _, fw := range frameworkNames[1:] { // skip Celeris
		summaries[fw] = &fwSummary{name: fw}
	}

	// Collect all comparison benchmarks.
	allBenches := append(middlewareNames, chainNames...)
	for _, bench := range allBenches {
		cKey := bench + "_Celeris"
		cNs := medians[cKey].NsPerOp
		if cNs == 0 {
			continue
		}
		for _, fw := range frameworkNames[1:] {
			key := bench + "_" + fw
			r, ok := medians[key]
			if !ok {
				continue
			}
			s := summaries[fw]
			s.total++
			delta := (r.NsPerOp - cNs) / cNs * 100
			s.avgDelta += delta
			if delta > 2 {
				s.wins++
			} else if delta < -2 {
				s.losses++
			}
		}
	}

	sb.WriteString(fmt.Sprintf("%-12s | %6s | %6s | %6s | %10s\n",
		"Framework", "Faster", "Slower", "Total", "Avg Delta"))
	sb.WriteString(strings.Repeat("-", 50) + "\n")

	for _, fw := range frameworkNames[1:] {
		s := summaries[fw]
		avg := 0.0
		if s.total > 0 {
			avg = s.avgDelta / float64(s.total)
		}
		// "Faster" means competitor is slower (positive delta = celeris wins).
		sb.WriteString(fmt.Sprintf("%-12s | %6d | %6d | %6d | %+9.0f%%\n",
			s.name, s.wins, s.losses, s.total, avg))
	}

	// Overall verdict.
	sb.WriteString("\n")
	allFaster := true
	for _, fw := range frameworkNames[1:] {
		s := summaries[fw]
		if s.total > 0 && s.avgDelta/float64(s.total) < 0 {
			allFaster = false
			break
		}
	}
	if allFaster {
		sb.WriteString("Verdict: Celeris middleware is faster than all competitors on average.\n")
	}

	// Report any regressions > 20% vs any framework.
	sb.WriteString("\nRegressions (Celeris >20%% slower than competitor):\n")
	hasRegression := false
	for _, bench := range allBenches {
		cKey := bench + "_Celeris"
		cNs := medians[cKey].NsPerOp
		if cNs == 0 {
			continue
		}
		for _, fw := range frameworkNames[1:] {
			key := bench + "_" + fw
			r, ok := medians[key]
			if !ok {
				continue
			}
			delta := (cNs - r.NsPerOp) / r.NsPerOp * 100
			if delta > 20 {
				sb.WriteString(fmt.Sprintf("  %s: Celeris %.0f ns vs %s %.0f ns (%+.0f%%)\n",
					bench, cNs, fw, r.NsPerOp, delta))
				hasRegression = true
			}
		}
	}
	if !hasRegression {
		sb.WriteString("  None.\n")
	}

	_ = math.Abs // keep import

	return sb.String()
}
