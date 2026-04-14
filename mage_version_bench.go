//go:build mage

package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// VersionBenchmark runs an N-way comparison across multiple celeris versions
// using wrk/h2load in a Multipass VM.
//
// Configure the ref list via the VERSIONS env var (comma-separated):
//
//	VERSIONS=v1.1.0,v1.2.0,v1.3.0,v1.3.3,HEAD mage versionBenchmark
//
// Default is: v1.1.0,v1.2.0,v1.3.0,v1.3.3,HEAD (HEAD = current working tree).
// Requires each ref to have test/benchmark/profiled/main.go with the ENGINE,
// PROTOCOL, and PORT env var interface. v1.0.0 is not compatible.
func VersionBenchmark() error {
	refs := parseVersionRefs(os.Getenv("VERSIONS"))
	if len(refs) < 2 {
		return fmt.Errorf("VersionBenchmark needs at least 2 refs, got %v", refs)
	}

	dir, err := resultsDir("version-benchmark")
	if err != nil {
		return err
	}
	arch := hostArch()

	fmt.Printf("Multi-Version Benchmark (%d refs, linux/%s)\n", len(refs), arch)
	fmt.Printf("Refs: %s\n\n", strings.Join(refs, ", "))

	bins := make(map[string]string)
	for _, ref := range refs {
		binPath := filepath.Join(dir, "profiled-"+sanitizeRef(ref))
		if ref == "HEAD" {
			if err := crossCompile("test/benchmark/profiled", binPath, arch); err != nil {
				return fmt.Errorf("build HEAD: %w", err)
			}
		} else {
			if err := crossCompileFromRef("refs/tags/"+ref, "test/benchmark/profiled", binPath, arch); err != nil {
				if err2 := crossCompileFromRef(ref, "test/benchmark/profiled", binPath, arch); err2 != nil {
					return fmt.Errorf("build %s: %w (fallback: %w)", ref, err, err2)
				}
			}
		}
		bins[ref] = binPath
	}

	return runVersionBenchInVM(dir, refs, bins, arch)
}

func sanitizeRef(ref string) string {
	r := strings.NewReplacer("/", "-", ".", "-", "+", "-")
	return r.Replace(ref)
}

func parseVersionRefs(spec string) []string {
	if spec == "" {
		// Bump to include v1.3.4 once that tag is published.
		return []string{"v1.1.0", "v1.2.0", "v1.3.0", "v1.3.3", "HEAD"}
	}
	parts := strings.Split(spec, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runVersionBenchInVM(dir string, refs []string, bins map[string]string, arch string) error {
	fmt.Println("Ensuring VM is ready...")
	if err := ensureVMWithSpecs(benchmarkVM, "6", "8G"); err != nil {
		return err
	}
	defer destroyVM(benchmarkVM)

	fmt.Println("Installing load tools...")
	_, _ = vmExec(benchmarkVM, "sudo dnf install -y -q wrk nghttp2 2>/dev/null || (sudo apt-get update -qq && sudo apt-get install -y -qq wrk nghttp2-client) 2>/dev/null || echo 'WARNING: could not install load tools'")
	_, _ = vmExec(benchmarkVM, "mkdir -p /tmp/bench")

	for _, ref := range refs {
		remote := "/tmp/bench/profiled-" + sanitizeRef(ref)
		fmt.Printf("Transferring binary for %s → %s\n", ref, remote)
		if err := transferToVM(benchmarkVM, bins[ref], remote); err != nil {
			return fmt.Errorf("transfer %s: %w", ref, err)
		}
	}
	_, _ = vmExec(benchmarkVM, "chmod +x /tmp/bench/profiled-*")

	const rounds = 3
	runsByRef := make(map[string][]map[string]float64)
	for _, ref := range refs {
		runsByRef[ref] = nil
	}

	// Interleaved schedule: round-robin by ref × rounds, so thermal/frequency
	// drift is distributed across all versions rather than biasing one.
	type pass struct {
		label     string
		ref       string
		serverBin string
	}
	var schedule []pass
	for round := 1; round <= rounds; round++ {
		for _, ref := range refs {
			schedule = append(schedule, pass{
				label:     fmt.Sprintf("%s R%d", ref, round),
				ref:       ref,
				serverBin: "/tmp/bench/profiled-" + sanitizeRef(ref),
			})
		}
	}

	for i, p := range schedule {
		fmt.Printf("\n--- Pass %d/%d: %s ---\n", i+1, len(schedule), p.label)
		script := buildBenchPassScript(p.serverBin)
		out, err := vmExec(benchmarkVM, script)
		if err != nil {
			fmt.Printf("WARNING: pass failed: %v\n", err)
		}
		fmt.Println(out)
		results := parseBenchOutput(out)
		runsByRef[p.ref] = append(runsByRef[p.ref], results)
	}

	report := generateVersionReport(refs, runsByRef)
	fmt.Println("\n" + report)
	reportPath := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(reportPath, []byte(report), 0o644); err != nil {
		return err
	}
	fmt.Printf("Report saved to: %s\n", reportPath)
	return nil
}

// generateVersionReport produces an N-way comparison table. Baseline is the
// first ref; deltas are expressed relative to baseline.
func generateVersionReport(refs []string, runsByRef map[string][]map[string]float64) string {
	// Collect medians per (ref, config).
	medians := make(map[string]map[string]float64) // medians[ref][config]
	for _, ref := range refs {
		vals := make(map[string][]float64)
		for _, run := range runsByRef[ref] {
			for cfg, rps := range run {
				vals[cfg] = append(vals[cfg], rps)
			}
		}
		m := make(map[string]float64)
		for cfg, v := range vals {
			m[cfg] = medianFloat64(v)
		}
		medians[ref] = m
	}

	var configs []string
	for _, cfg := range benchConfigs {
		configs = append(configs, fmt.Sprintf("%s-%s", cfg.engine, cfg.protocol))
	}

	baseline := refs[0]
	var sb strings.Builder
	sb.WriteString("Celeris Multi-Version Benchmark Report (wrk + h2load)\n")
	sb.WriteString(fmt.Sprintf("Refs: %s  (baseline = %s)\n", strings.Join(refs, ", "), baseline))
	sb.WriteString(fmt.Sprintf("Date: %s\n", time.Now().Format("2006-01-02 15:04:05")))
	sb.WriteString(fmt.Sprintf("Rounds: %d per ref (fully interleaved)\n", len(runsByRef[baseline])))
	sb.WriteString(fmt.Sprintf("H1 load: wrk -t4 -c256 -d%ds\n", loadDuration))
	sb.WriteString(fmt.Sprintf("H2 load: h2load -c128 -m128 -t4 -D%d\n", loadDuration))
	sb.WriteString("CPU pinning: server CPUs 0-3, load CPUs 4-7\n\n")

	// Header.
	sb.WriteString(fmt.Sprintf("%-20s", "Config"))
	for _, ref := range refs {
		sb.WriteString(fmt.Sprintf(" | %12s", ref))
	}
	for _, ref := range refs[1:] {
		sb.WriteString(fmt.Sprintf(" | %8s", "Δ"+ref))
	}
	sb.WriteString("\n")
	sb.WriteString(strings.Repeat("-", 20+(15*len(refs))+(11*(len(refs)-1))) + "\n")

	improvedByRef := make(map[string]int)
	neutralByRef := make(map[string]int)
	regressedByRef := make(map[string]int)
	totalDeltaByRef := make(map[string]float64)

	for _, cfg := range configs {
		sb.WriteString(fmt.Sprintf("%-20s", cfg))
		baseRPS := medians[baseline][cfg]
		for _, ref := range refs {
			rps := medians[ref][cfg]
			sb.WriteString(fmt.Sprintf(" | %12.0f", rps))
		}
		for _, ref := range refs[1:] {
			rps := medians[ref][cfg]
			if baseRPS > 0 && rps > 0 {
				d := (rps - baseRPS) / baseRPS * 100
				sb.WriteString(fmt.Sprintf(" | %+7.1f%%", d))
				totalDeltaByRef[ref] += d
				switch {
				case math.Abs(d) < 2.0:
					neutralByRef[ref]++
				case d > 0:
					improvedByRef[ref]++
				default:
					regressedByRef[ref]++
				}
			} else {
				sb.WriteString(fmt.Sprintf(" | %8s", "N/A"))
			}
		}
		sb.WriteString("\n")
	}
	sb.WriteString(strings.Repeat("-", 20+(15*len(refs))+(11*(len(refs)-1))) + "\n")

	sb.WriteString("\nPer-ref delta vs baseline:\n")
	n := len(configs)
	for _, ref := range refs[1:] {
		avg := 0.0
		if n > 0 {
			avg = totalDeltaByRef[ref] / float64(n)
		}
		sb.WriteString(fmt.Sprintf("  %-12s: %d improved, %d neutral, %d regressed (avg: %+.1f%%)\n",
			ref, improvedByRef[ref], neutralByRef[ref], regressedByRef[ref], avg))
	}

	// io_uring vs epoll per ref.
	sb.WriteString("\nio_uring vs epoll (per ref):\n")
	for _, proto := range []string{"h1", "h2"} {
		iouName := fmt.Sprintf("iouring-%s", proto)
		epoName := fmt.Sprintf("epoll-%s", proto)
		sb.WriteString(fmt.Sprintf("  %s:\n", proto))
		for _, ref := range refs {
			iou := medians[ref][iouName]
			epo := medians[ref][epoName]
			if epo > 0 {
				adv := (iou - epo) / epo * 100
				sb.WriteString(fmt.Sprintf("    %-12s io_uring %8.0f  epoll %8.0f  %+.1f%%\n",
					ref, iou, epo, adv))
			}
		}
	}

	return sb.String()
}
