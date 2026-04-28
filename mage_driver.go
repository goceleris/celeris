//go:build mage

// Package main — mage targets for the driver profile-driven
// optimization loop (PG / Redis / memcached).
//
// These targets gate correctness before benchmarking (PreBench), capture
// per-subsystem baselines (BaselineBench), deep-dive individual driver
// benchmarks with pprof artifacts (DriverProfile), and run the full
// comparator sweep (DriverBench). They follow the existing mage_*.go
// conventions: results land in results/<YYYYMMDD-HHMMSS>-<label>/, commands
// shell out via the shared run/output helpers, and each target returns an
// error that the mage runner surfaces.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// TestIntegration runs the integration test suite locally.
//
// Mirrors the ci.yml "integration" job so contributors can reproduce it
// without pushing a branch.
func TestIntegration() error {
	return run("go", "test", "-race", "-count=1", "-timeout=120s", "./test/integration/...")
}

// H2CCompliance runs the h2c upgrade compliance tests under ./test/spec/...
//
// The tests are linux-only (build tag); on non-linux hosts mage skips them
// with a message rather than failing, matching how the h2spec gate is
// treated elsewhere.
func H2CCompliance() error {
	if runtime.GOOS != "linux" {
		fmt.Println("H2CCompliance: skipping (linux-only engines needed)")
		return nil
	}
	return run("go", "test",
		"-count=1", "-timeout=120s",
		"-run", "TestH2CUpgrade",
		"./test/spec/...")
}

// TestDriver runs the driver conformance suite against a live server.
//
// Usage: mage testDriver postgres   (requires CELERIS_PG_DSN)
//
//	mage testDriver redis      (defaults CELERIS_REDIS_ADDR=127.0.0.1:6379)
//
// When a required env var is missing, TestDriver prints a pointer at the
// docker-compose.yml under the relevant conformance directory so the user
// can bring a server up locally.
func TestDriver(driver string) error {
	driver = strings.ToLower(strings.TrimSpace(driver))
	switch driver {
	case "postgres":
		if os.Getenv("CELERIS_PG_DSN") == "" {
			fmt.Println("CELERIS_PG_DSN is required for mage testDriver postgres.")
			fmt.Println("Start a local server via:")
			fmt.Println("  docker compose -f test/conformance/postgres/docker-compose.yml up -d")
			fmt.Println("  export CELERIS_PG_DSN='postgres://celeris:celeris@127.0.0.1:5432/celeristest?sslmode=disable'")
			return fmt.Errorf("CELERIS_PG_DSN not set")
		}
		return run("go", "test",
			"-tags", "postgres",
			"-race", "-count=1", "-timeout=300s",
			"./test/conformance/postgres/...")
	case "redis":
		env := map[string]string{}
		if os.Getenv("CELERIS_REDIS_ADDR") == "" {
			env["CELERIS_REDIS_ADDR"] = "127.0.0.1:6379"
		}
		return runEnv(env, "go", "test",
			"-tags", "redis",
			"-race", "-count=1", "-timeout=300s",
			"./test/conformance/redis/...")
	case "memcached":
		env := map[string]string{}
		if os.Getenv("CELERIS_MEMCACHED_ADDR") == "" {
			env["CELERIS_MEMCACHED_ADDR"] = "127.0.0.1:11211"
		}
		return runEnv(env, "go", "test",
			"-tags", "memcached",
			"-race", "-count=1", "-timeout=300s",
			"./test/conformance/memcached/...")
	default:
		return fmt.Errorf("unknown driver %q (want postgres|redis|memcached)", driver)
	}
}

// RedisSpec runs the RESP2/RESP3 protocol compliance verification suite
// against a live Redis server.
//
// Usage:
//
//	mage redisSpec                                    # uses 127.0.0.1:6379
//	CELERIS_REDIS_ADDR=host:port mage redisSpec       # explicit address
//
// Requires a reachable Redis 7.2+ instance. AUTH tests additionally require
// CELERIS_REDIS_PASSWORD.
func RedisSpec() error {
	env := map[string]string{}
	if os.Getenv("CELERIS_REDIS_ADDR") == "" {
		env["CELERIS_REDIS_ADDR"] = "127.0.0.1:6379"
	}
	return runEnv(env, "go", "test",
		"-tags", "redisspec",
		"-race", "-count=1", "-timeout=300s", "-v",
		"./test/redisspec/...")
}

// MCSpec runs the memcached text + binary protocol compliance verification
// suite against a live memcached server.
//
// Usage:
//
//	mage mcSpec                                     # uses 127.0.0.1:11211
//	CELERIS_MEMCACHED_ADDR=host:port mage mcSpec    # explicit address
//
// Requires a reachable memcached 1.6+ instance with both text and binary
// protocols available.
func MCSpec() error {
	env := map[string]string{}
	if os.Getenv("CELERIS_MEMCACHED_ADDR") == "" {
		env["CELERIS_MEMCACHED_ADDR"] = "127.0.0.1:11211"
	}
	return runEnv(env, "go", "test",
		"-tags", "memcached",
		"-race", "-count=1", "-timeout=300s", "-v",
		"./test/mcspec/...")
}

// PGSpec runs the PostgreSQL v3 wire protocol compliance verification suite.
//
// Requires CELERIS_PG_DSN pointing to a reachable PostgreSQL 16+ server.
// Tests exercise raw wire framing, message types, and state transitions
// against a live server, following the h2spec/Autobahn pattern.
//
// Usage:
//
//	export CELERIS_PG_DSN='postgres://postgres:celeris@127.0.0.1:5432/celeristest?sslmode=disable'
//	mage pgSpec
func PGSpec() error {
	if os.Getenv("CELERIS_PG_DSN") == "" {
		fmt.Println("CELERIS_PG_DSN is required for mage pgSpec.")
		fmt.Println("Start a local server via:")
		fmt.Println("  docker compose -f test/conformance/postgres/docker-compose.yml up -d")
		fmt.Println("  export CELERIS_PG_DSN='postgres://celeris:celeris@127.0.0.1:5432/celeristest?sslmode=disable'")
		return fmt.Errorf("CELERIS_PG_DSN not set")
	}
	return run("go", "test",
		"-tags", "pgspec",
		"-count=1", "-timeout=300s", "-v",
		"./test/pgspec/...")
}

// PreBench runs the full correctness gate before any benchmarking.
//
// Executes, in order: Lint → Test → Spec → TestIntegration → H2CCompliance
// → TestDriver postgres → TestDriver redis. Fails fast on the first error
// so a broken change doesn't burn benchmark time.
//
// The two TestDriver steps require their respective services (Postgres /
// Redis); if you're running PreBench locally without docker, run the
// subset you care about directly.
func PreBench() error {
	steps := []struct {
		name string
		fn   func() error
	}{
		{"lint", Lint},
		{"test", Test},
		{"spec", Spec},
		{"integration", TestIntegration},
		{"h2c-compliance", H2CCompliance},
		{"driver-postgres", func() error { return TestDriver("postgres") }},
		{"driver-redis", func() error { return TestDriver("redis") }},
	}
	for _, s := range steps {
		fmt.Printf("\n=== PreBench: %s ===\n", s.name)
		if err := s.fn(); err != nil {
			return fmt.Errorf("prebench %s: %w", s.name, err)
		}
	}
	fmt.Println("\n=== PreBench: all gates passed ===")
	return nil
}

// BaselineBench captures a baseline benchmark for a single subsystem.
//
// Usage: mage baselineBench <subsys>  where <subsys> ∈
// {eventloop, h2c, postgres, redis}. Output lands in
// results/<ts>-baseline-<subsys>/ with:
//
//   - bench.txt — raw `go test -bench` output
//   - env.json  — git rev, Go version, uname -a, /proc/cpuinfo (if present)
//
// This is the regression gate — one focused benchmark per subsystem — and
// is distinct from DriverBench, which runs the full comparator sweep.
func BaselineBench(subsys string) error {
	subsys = strings.ToLower(strings.TrimSpace(subsys))
	dir, err := resultsDir("baseline-" + subsys)
	if err != nil {
		return err
	}

	var (
		benchOut string
		runErr   error
	)
	switch subsys {
	case "eventloop":
		benchOut, runErr = output("go", "test",
			"-run=^$",
			"-bench", "BenchmarkHTTPWith|BenchmarkHTTPNoProvider",
			"-benchmem", "-count=1",
			"./engine/...")
	case "h2c":
		benchOut, runErr = output("go", "test",
			"-run=^$",
			"-bench", "BenchmarkH2C",
			"-benchmem", "-count=1",
			"./test/spec/...")
	case "postgres":
		benchOut, runErr = outputInDir("test/drivercmp/postgres",
			"go", "test", "-run=^$", "-bench", ".",
			"-benchmem", "-count=1", "./...")
	case "redis":
		benchOut, runErr = outputInDir("test/drivercmp/redis",
			"go", "test", "-run=^$", "-bench", ".",
			"-benchmem", "-count=1", "./...")
	case "memcached":
		benchOut, runErr = outputInDir("test/drivercmp/memcached",
			"go", "test", "-run=^$", "-bench", ".",
			"-benchmem", "-count=1", "./...")
	default:
		return fmt.Errorf("unknown subsys %q (want eventloop|h2c|postgres|redis|memcached)", subsys)
	}

	benchPath := filepath.Join(dir, "bench.txt")
	if err := os.WriteFile(benchPath, []byte(benchOut), 0o644); err != nil {
		return fmt.Errorf("write bench.txt: %w", err)
	}
	fmt.Println(benchOut)
	fmt.Printf("Bench output: %s\n", benchPath)

	if err := writeEnvJSON(dir, subsys); err != nil {
		fmt.Printf("WARNING: env.json: %v\n", err)
	}

	return runErr
}

// DriverProfile captures a full set of pprof profiles for one comparator
// benchmark. Usage: mage driverProfile <driver> <benchName> [duration].
//
// <driver> ∈ {postgres, redis}; <benchName> is the Go -bench regex (e.g.
// "BenchmarkGet_Celeris"). duration defaults to 30s. The benchmark is
// re-run four times, once per profile kind, because only one pprof output
// can be collected per go test invocation.
//
// Outputs:
//
//	cpu.pprof / heap.pprof / mutex.pprof / block.pprof
//	cpu.top.txt    — go tool pprof -top -cum -nodecount=30 cpu.pprof
//	alloc.top.txt  — go tool pprof -alloc_objects -top -nodecount=30 heap.pprof
//	bench.txt      — combined -bench output from all four runs
func DriverProfile(driver, benchName, duration string) error {
	driver = strings.ToLower(strings.TrimSpace(driver))
	var benchDir string
	switch driver {
	case "postgres":
		benchDir = "test/drivercmp/postgres"
	case "redis":
		benchDir = "test/drivercmp/redis"
	case "memcached":
		benchDir = "test/drivercmp/memcached"
	default:
		return fmt.Errorf("unknown driver %q (want postgres|redis|memcached)", driver)
	}
	if strings.TrimSpace(duration) == "" {
		duration = "30s"
	}

	dir, err := resultsDir(fmt.Sprintf("profile-%s-%s", driver, sanitizeLabel(benchName)))
	if err != nil {
		return err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	var combined strings.Builder
	profiles := []struct {
		flag string // -cpuprofile / -memprofile / ...
		file string // output filename
	}{
		{"-cpuprofile", "cpu.pprof"},
		{"-memprofile", "heap.pprof"},
		{"-mutexprofile", "mutex.pprof"},
		{"-blockprofile", "block.pprof"},
	}
	for _, p := range profiles {
		outPath := filepath.Join(absDir, p.file)
		fmt.Printf("--- Profile pass: %s (%s) ---\n", p.file, duration)
		testArgs := []string{
			"test",
			"-run=^$",
			"-bench", benchName,
			"-benchmem",
			"-benchtime", duration,
			"-count=1",
			p.flag, outPath,
			"./...",
		}
		out, err := outputInDir(benchDir, "go", testArgs...)
		if err != nil {
			fmt.Printf("WARNING: %s pass failed: %v\n", p.file, err)
		}
		combined.WriteString(fmt.Sprintf(">>> pass=%s\n", p.file))
		combined.WriteString(out)
		combined.WriteString("\n\n")
	}

	if err := os.WriteFile(filepath.Join(dir, "bench.txt"), []byte(combined.String()), 0o644); err != nil {
		return fmt.Errorf("write bench.txt: %w", err)
	}

	// Derive text summaries.
	cpuProf := filepath.Join(dir, "cpu.pprof")
	if fi, err := os.Stat(cpuProf); err == nil && fi.Size() > 0 {
		if out, err := output("go", "tool", "pprof", "-top", "-cum", "-nodecount=30", cpuProf); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "cpu.top.txt"), []byte(out), 0o644)
		}
	}
	heapProf := filepath.Join(dir, "heap.pprof")
	if fi, err := os.Stat(heapProf); err == nil && fi.Size() > 0 {
		if out, err := output("go", "tool", "pprof", "-alloc_objects", "-top", "-nodecount=30", heapProf); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "alloc.top.txt"), []byte(out), 0o644)
		}
	}

	fmt.Printf("Profiles saved to: %s\n", dir)
	return nil
}

// DriverBench runs the full comparator benchmark sweep for a driver.
//
// Usage: mage driverBench <driver>  where <driver> ∈ {postgres, redis}.
// Writes raw output to results/<ts>-bench-<driver>/bench.txt. This is the
// "full sweep" variant; BaselineBench is the same thing with a focused
// regex for regression tracking.
func DriverBench(driver string) error {
	driver = strings.ToLower(strings.TrimSpace(driver))
	var benchDir string
	switch driver {
	case "postgres":
		benchDir = "test/drivercmp/postgres"
	case "redis":
		benchDir = "test/drivercmp/redis"
	case "memcached":
		benchDir = "test/drivercmp/memcached"
	default:
		return fmt.Errorf("unknown driver %q (want postgres|redis|memcached)", driver)
	}
	dir, err := resultsDir("bench-" + driver)
	if err != nil {
		return err
	}
	out, err := outputInDir(benchDir, "go", "test",
		"-run=^$", "-bench", ".", "-benchmem", "-count=1", "./...")
	benchPath := filepath.Join(dir, "bench.txt")
	if wErr := os.WriteFile(benchPath, []byte(out), 0o644); wErr != nil {
		return fmt.Errorf("write bench.txt: %w", wErr)
	}
	fmt.Println(out)
	fmt.Printf("Bench output: %s\n", benchPath)

	if envErr := writeEnvJSON(dir, "driver-"+driver); envErr != nil {
		fmt.Printf("WARNING: env.json: %v\n", envErr)
	}
	return err
}

// --- helpers ---

// outputInDir runs a command in the specified working directory and
// returns combined stdout. Used so the drivercmp go.mod submodules resolve
// their own deps without touching the root module.
func outputInDir(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// writeEnvJSON captures the environment fingerprint for a benchmark run.
// Best-effort — missing pieces (e.g. /proc/cpuinfo on macOS) are recorded
// as empty strings rather than failing the run.
func writeEnvJSON(dir, label string) error {
	rev, _ := output("git", "rev-parse", "HEAD")
	goVer, _ := output("go", "version")
	uname, _ := output("uname", "-a")
	cpuinfo := ""
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		cpuinfo = string(data)
	}
	// Cap cpuinfo to avoid blowing up the file on many-core hosts.
	if len(cpuinfo) > 64*1024 {
		cpuinfo = cpuinfo[:64*1024] + "\n[truncated]"
	}

	// Simple hand-rolled JSON to avoid an encoding/json dep dance in mage
	// build-tagged code (and so the output is deterministic / reviewable).
	var sb strings.Builder
	sb.WriteString("{\n")
	writeJSONField(&sb, "label", label, true)
	writeJSONField(&sb, "date", timeNowRFC3339(), true)
	writeJSONField(&sb, "git_rev", rev, true)
	writeJSONField(&sb, "go_version", goVer, true)
	writeJSONField(&sb, "uname", uname, true)
	writeJSONField(&sb, "goos", runtime.GOOS, true)
	writeJSONField(&sb, "goarch", runtime.GOARCH, true)
	writeJSONField(&sb, "cpuinfo", cpuinfo, false)
	sb.WriteString("\n}\n")

	return os.WriteFile(filepath.Join(dir, "env.json"), []byte(sb.String()), 0o644)
}

func writeJSONField(sb *strings.Builder, key, val string, trailingComma bool) {
	sb.WriteString("  ")
	sb.WriteString(jsonQuote(key))
	sb.WriteString(": ")
	sb.WriteString(jsonQuote(val))
	if trailingComma {
		sb.WriteString(",")
	}
	sb.WriteString("\n")
}

func jsonQuote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&sb, `\u%04x`, r)
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

// sanitizeLabel converts a benchmark regex into a filesystem-safe label.
func sanitizeLabel(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	if sb.Len() == 0 {
		return "all"
	}
	return sb.String()
}

// timeNowRFC3339 returns the current time formatted as RFC3339 for env.json.
func timeNowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
