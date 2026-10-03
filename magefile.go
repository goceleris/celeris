//go:build mage

package main

import (
	"debug/buildinfo"
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

// API rewrites the API golden files under api/ from the exported API of
// every package (api/README.md). Run it in the PR that changes an exported
// identifier and commit api/ with the change; CI fails while api/ is stale.
func API() error {
	return run("go", "-C", ".github/tools", "run", "./apidump", "-w")
}

// APICheck checks api/ against the exported API without writing it, as CI
// does, and prints the diff when it is stale.
func APICheck() error {
	return run("go", "-C", ".github/tools", "run", "./apidump")
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
	if err := run("go", "test", "-fuzz=FuzzParseRequest", "-fuzztime="+duration, "./internal/protocol/h1/"); err != nil {
		return err
	}
	return run("go", "test", "-fuzz=FuzzParseChunkedBody", "-fuzztime="+duration, "./internal/protocol/h1/")
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

// The h2spec that `mage Tools` installs (celeris#838). h2spec's tags after
// v2.2.1 carry a go.mod without the /v2 suffix, so the module proxy has no
// version for them: `go install .../h2spec@v2.6.0` is refused, and `@latest`
// resolves to v2.2.1+incompatible, an older test set. The commit the v2.6.0
// tag points at is still reachable as a pseudo-version, and building it with
// the -ldflags of h2spec's own Makefile gives the release's code and its
// version string, checked against the Go checksum database like any module, for
// every GOOS/GOARCH (the release ships amd64 binaries only).
//
// The version string is the -ldflags stamp, so it would say 2.6.0 for any
// code. What proves the code is the module version the binary records in
// its build info, which Tools checks against h2specModVersion.
const (
	h2specVersion    = "2.6.0"
	h2specCommit     = "70ac2294010887f48b18e2d64f5cccd48421fad1"
	h2specMod        = "github.com/summerwind/h2spec"
	h2specModVersion = "v1.5.1-0.20200804131034-70ac22940108"
	h2specPkg        = h2specMod + "/cmd/h2spec@" + h2specModVersion
	h2specRelease    = "https://github.com/summerwind/h2spec/releases/tag/v2.6.0"
)

// Tools installs external test tools (h2spec 2.6.0) and prints the version
// it ended with. It fails, rather than settle for another version, when it
// cannot install 2.6.0 or when PATH finds a different h2spec first.
func Tools() error {
	if path, err := exec.LookPath("h2spec"); err == nil {
		v := h2specVersionOf(path)
		if v == h2specVersion {
			fmt.Printf("h2spec: %s at %s (already installed)\n", v, path)
			return nil
		}
		fmt.Printf("h2spec: %s reports version %q, not %s; installing %s\n", path, v, h2specVersion, h2specVersion)
	}
	fmt.Printf("Installing h2spec %s: go install %s\n", h2specVersion, h2specPkg)
	ldflags := fmt.Sprintf("-ldflags=-X main.VERSION=%s -X main.COMMIT=%s", h2specVersion, h2specCommit)
	if err := run("go", "install", ldflags, h2specPkg); err != nil {
		return fmt.Errorf("h2spec %s: go install failed (%w); download the release binary from %s and put it on PATH", h2specVersion, err, h2specRelease)
	}
	binDir, err := goBinDir()
	if err != nil {
		return err
	}
	exe := "h2spec"
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	installed := filepath.Join(binDir, exe)
	if v := h2specVersionOf(installed); v != h2specVersion {
		return fmt.Errorf("h2spec: %s reports version %q after the install, not %s; download the release binary from %s", installed, v, h2specVersion, h2specRelease)
	}
	built, err := h2specBuiltFrom(installed)
	if err != nil {
		return fmt.Errorf("h2spec: cannot read the build info of %s: %w", installed, err)
	}
	if want := h2specMod + "@" + h2specModVersion; built != want {
		return fmt.Errorf("h2spec: %s was built from %s, not %s (the v%s commit); its version string is only the -ldflags stamp", installed, built, want, h2specVersion)
	}
	path, err := exec.LookPath("h2spec")
	if err != nil {
		return fmt.Errorf("h2spec %s is at %s, but %s is not on PATH, so TestH2Spec would skip; add it to PATH", h2specVersion, installed, binDir)
	}
	if v := h2specVersionOf(path); v != h2specVersion {
		return fmt.Errorf("h2spec %s is at %s, but PATH finds %s (version %q) first; remove it or put %s first on PATH", h2specVersion, installed, path, v, binDir)
	}
	if sameFile(path, installed) {
		fmt.Printf("h2spec: %s at %s (built from %s)\n", h2specVersion, path, built)
	} else {
		fmt.Printf("h2spec: %s at %s, first on PATH (the install at %s is built from %s)\n", h2specVersion, path, installed, built)
	}
	return nil
}

func sameFile(a, b string) bool {
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}

// h2specBuiltFrom returns the main module path@version that a Go-built
// binary records in its build info.
func h2specBuiltFrom(path string) (string, error) {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", err
	}
	return bi.Main.Path + "@" + bi.Main.Version, nil
}

// h2specVersionOf returns the version an h2spec binary reports
// ("Version: 2.6.0 (<commit>)" gives "2.6.0"), or "" when it cannot run.
func h2specVersionOf(path string) string {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return ""
	}
	v, ok := strings.CutPrefix(strings.TrimSpace(string(out)), "Version: ")
	if !ok {
		return ""
	}
	v, _, _ = strings.Cut(v, " ")
	return v
}

// goBinDir returns the directory `go install` writes to: GOBIN, or the bin
// directory of GOPATH's first entry.
func goBinDir() (string, error) {
	gobin, err := output("go", "env", "GOBIN")
	if err != nil {
		return "", fmt.Errorf("go env GOBIN: %w", err)
	}
	if gobin != "" {
		return gobin, nil
	}
	gopath, err := output("go", "env", "GOPATH")
	if err != nil {
		return "", fmt.Errorf("go env GOPATH: %w", err)
	}
	return filepath.Join(filepath.SplitList(gopath)[0], "bin"), nil
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
