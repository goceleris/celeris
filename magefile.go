//go:build mage

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

// Default is the target invoked when `mage` runs with no arguments.
// It runs lint, tests, and a full build via [All].
var Default = All

// All runs lint, test, and build.
func All() {
	must(Lint())
	must(Test())
	must(Build())
}

// Lint runs golangci-lint.
func Lint() error {
	return run("golangci-lint", "run", "./...")
}

// Test runs all tests with the race detector.
func Test() error {
	return run("go", "test", "-race", "-count=1", "./...")
}

// Build compiles all packages.
func Build() error {
	return run("go", "build", "./...")
}

// Bench runs all benchmarks.
func Bench() error {
	return run("go", "test", "-bench=.", "-benchmem", "-run=^$", "./...")
}

// Fuzz runs fuzz tests for the specified duration (default 30s).
func Fuzz() error {
	duration := "30s"
	if d := os.Getenv("FUZZ_TIME"); d != "" {
		duration = d
	}
	if err := run("go", "test", "-fuzz=FuzzParseRequest", "-fuzztime="+duration, "./protocol/h1/"); err != nil {
		return err
	}
	return run("go", "test", "-fuzz=FuzzParseChunkedBody", "-fuzztime="+duration, "./protocol/h1/")
}

// Clean removes build artifacts.
func Clean() error {
	return run("go", "clean", "./...")
}

// CleanBenchmarks removes stale benchmark JSON files from the project root.
func CleanBenchmarks() error {
	matches, _ := filepath.Glob("*-benchmarks.json")
	if len(matches) == 0 {
		fmt.Println("No benchmark JSON files to clean.")
		return nil
	}
	for _, m := range matches {
		fmt.Printf("Removing %s\n", m)
		if err := os.Remove(m); err != nil {
			return err
		}
	}
	fmt.Printf("Removed %d benchmark file(s).\n", len(matches))
	return nil
}

// Tools installs external test tools (h2spec).
func Tools() error {
	if _, err := exec.LookPath("h2spec"); err == nil {
		fmt.Println("h2spec: already installed")
		return nil
	}
	fmt.Println("Installing h2spec v2.6.0...")

	goos := runtime.GOOS
	goarch := "amd64"

	if goos != "linux" && goos != "darwin" {
		fmt.Println("No prebuilt h2spec binary; trying go install...")
		return run("go", "install", "github.com/summerwind/h2spec/cmd/h2spec@latest")
	}

	gopath, err := output("go", "env", "GOPATH")
	if err != nil {
		return fmt.Errorf("GOPATH: %w", err)
	}
	binDir := filepath.Join(gopath, "bin")

	tarball := fmt.Sprintf("h2spec_%s_%s.tar.gz", goos, goarch)
	url := fmt.Sprintf("https://github.com/summerwind/h2spec/releases/download/v2.6.0/%s", tarball)

	if err := run("bash", "-c",
		fmt.Sprintf("curl -fsSL '%s' | tar xz -C '%s' h2spec", url, binDir)); err != nil {
		fmt.Println("Binary download failed; trying go install...")
		return run("go", "install", "github.com/summerwind/h2spec/cmd/h2spec@latest")
	}
	return nil
}

// H2Spec runs HTTP/2 conformance tests using h2spec across all engines.
func H2Spec() error {
	return run("go", "test", "-v", "-run", "TestH2Spec", "-count=1", "-timeout=120s", "./test/spec/...")
}

// H1Spec runs HTTP/1.1 RFC 9112 compliance tests across all engines.
func H1Spec() error {
	return run("go", "test", "-v", "-run", "TestH1Spec", "-count=1", "-timeout=120s", "./test/spec/...")
}

// Spec runs all protocol compliance tests (h2spec + h1spec) across all engines.
func Spec() error {
	return run("go", "test", "-v", "-count=1", "-timeout=120s", "./test/spec/...")
}

// TestAutobahn runs the Autobahn|Testsuite fuzzingclient against the
// celeris WebSocket middleware on each available engine. Requires Docker
// (for the autobahn-testsuite container) and Go to build the local
// echo-server binary. On macOS only the std engine is exercised.
//
// Reports land in test/autobahn/reports/clients/index.html.
func TestAutobahn() error {
	return runEnv(nil, "make", "-C", "test/autobahn", "autobahn")
}

// TestSoak runs the 5-minute WebSocket slow-consumer soak test. Validates
// that the engine-integrated backpressure pipeline keeps goroutine count
// and heap allocation bounded under sustained load. Override the
// duration via SOAK_DURATION (e.g. SOAK_DURATION=30m for the pre-release
// soak).
func TestSoak() error {
	duration := os.Getenv("SOAK_DURATION")
	if duration == "" {
		duration = "5m"
	}
	// Give the Go test framework a little slack on top of SOAK_DURATION.
	timeout := duration + "+5m"
	if d, err := time.ParseDuration(duration); err == nil {
		timeout = (d + 5*time.Minute).String()
	}
	return runEnv(map[string]string{"SOAK_DURATION": duration},
		"go", "test", "-tags=soak", "-timeout", timeout,
		"-run", "TestSoakSlowConsumer",
		"-v", "./middleware/websocket/...")
}

// BenchcmpSSE runs the head-to-head SSE benchmark suite at
// test/benchcmp_sse, comparing celeris's middleware/sse Broker against
// other Go SSE libraries (currently tmaxmax/go-sse). The directory is a
// separate Go module so competitor deps stay isolated. Override the
// benchmark count via BENCHCMP_COUNT (default 5) and benchtime via
// BENCHCMP_BENCHTIME (default 3s).
func BenchcmpSSE() error {
	count := os.Getenv("BENCHCMP_COUNT")
	if count == "" {
		count = "5"
	}
	benchtime := os.Getenv("BENCHCMP_BENCHTIME")
	if benchtime == "" {
		benchtime = "3s"
	}
	cmd := exec.Command("go", "test",
		"-bench", ".",
		"-benchmem",
		"-count", count,
		"-benchtime", benchtime,
		"-run", "^$",
		"./...")
	cmd.Dir = "test/benchcmp_sse"
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}

// BenchcmpWS runs the head-to-head WebSocket benchmark suite at
// test/benchcmp_ws against gorilla/websocket. Same env knobs as
// BenchcmpSSE.
func BenchcmpWS() error {
	count := os.Getenv("BENCHCMP_COUNT")
	if count == "" {
		count = "5"
	}
	benchtime := os.Getenv("BENCHCMP_BENCHTIME")
	if benchtime == "" {
		benchtime = "3s"
	}
	cmd := exec.Command("go", "test",
		"-bench", ".",
		"-benchmem",
		"-count", count,
		"-benchtime", benchtime,
		"-run", "^$",
		"./...")
	cmd.Dir = "test/benchcmp_ws"
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}

// LintLinux runs golangci-lint for Linux cross-compilation.
func LintLinux() error {
	return runEnv(map[string]string{"GOOS": "linux", "GOARCH": "amd64"}, "golangci-lint", "run", "./...")
}

// Check runs the full verification suite: lint, tests, spec compliance, and build.
func Check() {
	must(Lint())
	must(Test())
	must(Spec())
	must(Build())
}

// run executes a command with stdout/stderr connected to the terminal.
func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runEnv executes a command with extra environment variables.
func runEnv(env map[string]string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd.Run()
}

// output runs a command and returns its trimmed stdout.
func output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// must panics on error (used for targets that don't return error).
func must(err error) {
	if err != nil {
		panic(err)
	}
}
