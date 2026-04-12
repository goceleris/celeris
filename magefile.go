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

const vmName = "celeris-bench"

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

// goVersion reads the Go version from go.mod.
func goVersion() (string, error) {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		return "", fmt.Errorf("reading go.mod: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "go ") {
			return strings.TrimPrefix(line, "go "), nil
		}
	}
	return "", fmt.Errorf("go directive not found in go.mod")
}

// vmArch returns the architecture string for the Go download URL.
func vmArch() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}

// installGoScript returns a shell script that downloads and installs Go.
func installGoScript(goVer, arch string) string {
	tarball := fmt.Sprintf("go%s.linux-%s.tar.gz", goVer, arch)
	url := fmt.Sprintf("https://go.dev/dl/%s", tarball)
	return strings.Join([]string{
		"set -e",
		"cd /tmp",
		fmt.Sprintf("curl -fsSL -o /tmp/%s %s", tarball, url),
		fmt.Sprintf("sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/%s", tarball),
		fmt.Sprintf("rm /tmp/%s", tarball),
		`grep -q '/usr/local/go/bin' /home/ubuntu/.profile 2>/dev/null || echo 'export PATH=$PATH:/usr/local/go/bin' >> /home/ubuntu/.profile`,
		"/usr/local/go/bin/go version",
	}, " && ")
}

func ensureVM() error {
	goVer, err := goVersion()
	if err != nil {
		return err
	}
	arch := vmArch()

	if !multipassVMExists(vmName) {
		fmt.Printf("Creating Multipass VM (Go %s, linux/%s)...\n", goVer, arch)
		if err := run("multipass", "launch", "--name", vmName,
			"--cpus", "4", "--memory", "4G", "--disk", "20G", "noble"); err != nil {
			return fmt.Errorf("failed to create VM: %w", err)
		}
		fmt.Println("Installing Go from go.dev...")
		script := installGoScript(goVer, arch)
		if err := run("multipass", "exec", vmName, "--", "bash", "-c", script); err != nil {
			return fmt.Errorf("Go install failed: %w", err)
		}
	}

	return ensureVMRunning(vmName)
}

const vmProjectDir = "/home/ubuntu/celeris-src"

func syncSource() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	fmt.Println("Syncing source to VM...")
	_ = run("multipass", "umount", vmName+":celeris")
	_ = run("multipass", "exec", vmName, "--", "rm", "-rf", vmProjectDir)
	if err := run("multipass", "transfer", "-r", cwd, vmName+":"+vmProjectDir); err != nil {
		return fmt.Errorf("failed to sync source: %w", err)
	}
	return nil
}

// BenchLinux runs benchmarks inside a Multipass VM for Linux-accurate results.
func BenchLinux() error {
	if runtime.GOOS == "linux" {
		fmt.Println("Already on Linux, running benchmarks directly.")
		return Bench()
	}

	if err := ensureVM(); err != nil {
		return err
	}
	if err := syncSource(); err != nil {
		return err
	}

	fmt.Println("Running benchmarks in Linux VM...")
	return run("multipass", "exec", vmName, "--", "bash", "-c",
		"export PATH=$PATH:/usr/local/go/bin && cd "+vmProjectDir+" && go test -bench=. -benchmem -run='^$' ./...")
}

// TestLinux runs the full test suite inside a Multipass Linux VM.
func TestLinux() error {
	if runtime.GOOS == "linux" {
		fmt.Println("Already on Linux, running tests directly.")
		return Test()
	}

	if err := ensureVM(); err != nil {
		return err
	}
	if err := syncSource(); err != nil {
		return err
	}

	fmt.Println("Running tests in Linux VM...")
	return run("multipass", "exec", vmName, "--", "bash", "-c",
		"export PATH=$PATH:/usr/local/go/bin && cd "+vmProjectDir+" && go test -race -count=1 ./...")
}

// SpecLinux runs the full h2spec + h1spec compliance suite inside a Multipass Linux VM.
func SpecLinux() error {
	if runtime.GOOS == "linux" {
		fmt.Println("Already on Linux, running spec directly.")
		must(Tools())
		return Spec()
	}

	if err := ensureVM(); err != nil {
		return err
	}
	if err := syncSource(); err != nil {
		return err
	}
	if err := ensureH2SpecInVM(); err != nil {
		return err
	}

	fmt.Println("Running spec compliance suite in Linux VM...")
	return run("multipass", "exec", vmName, "--", "bash", "-c",
		"export PATH=$PATH:/usr/local/go/bin:/usr/local/bin && cd "+vmProjectDir+" && "+
			"go test -v -count=1 -timeout=300s ./test/spec/...")
}

// CheckLinux runs the full verification suite inside a Linux VM.
func CheckLinux() error {
	if runtime.GOOS == "linux" {
		fmt.Println("Already on Linux.")
		Check()
		return nil
	}

	if err := ensureVM(); err != nil {
		return err
	}
	if err := syncSource(); err != nil {
		return err
	}
	if err := ensureH2SpecInVM(); err != nil {
		return err
	}

	fmt.Println("Running full check in Linux VM...")
	return run("multipass", "exec", vmName, "--", "bash", "-c",
		"export PATH=$PATH:/usr/local/go/bin:/usr/local/bin && cd "+vmProjectDir+" && "+
			"go test -race -count=1 ./... && "+
			"go test -v -count=1 -timeout=300s ./test/spec/...")
}

// VMStop stops the Multipass benchmark VM.
func VMStop() error {
	return run("multipass", "stop", vmName)
}

// VMDelete deletes the Multipass benchmark VM.
func VMDelete() error {
	if err := run("multipass", "delete", vmName); err != nil {
		return err
	}
	return run("multipass", "purge")
}

func multipassVMExists(name string) bool {
	out, err := exec.Command("multipass", "list", "--format", "csv").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, name+",") {
			return true
		}
	}
	return false
}

func ensureH2SpecInVM() error {
	if err := exec.Command("multipass", "exec", vmName, "--", "which", "h2spec").Run(); err == nil {
		fmt.Println("h2spec: already installed in VM")
		return nil
	}
	fmt.Println("Installing h2spec in VM...")
	arch := vmArch()
	if arch == "arm64" {
		return run("multipass", "exec", vmName, "--", "bash", "-c",
			"cd /tmp && export GOPATH=/tmp/gopath && mkdir -p $GOPATH && "+
				"/usr/local/go/bin/go install github.com/summerwind/h2spec/cmd/h2spec@latest && "+
				"sudo cp /tmp/gopath/bin/h2spec /usr/local/bin/h2spec")
	}
	tarball := fmt.Sprintf("h2spec_linux_%s.tar.gz", arch)
	url := fmt.Sprintf("https://github.com/summerwind/h2spec/releases/download/v2.6.0/%s", tarball)
	return run("multipass", "exec", vmName, "--", "bash", "-c",
		fmt.Sprintf("curl -fsSL %s | sudo tar xz -C /usr/local/bin h2spec", url))
}

func ensureVMRunning(name string) error {
	out, err := exec.Command("multipass", "list", "--format", "csv").Output()
	if err != nil {
		return fmt.Errorf("failed to list VMs: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, name+",") {
			if strings.Contains(line, "Stopped") {
				fmt.Println("Starting VM...")
				return run("multipass", "start", name)
			}
			return nil
		}
	}
	return fmt.Errorf("VM %s not found", name)
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
