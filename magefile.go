//go:build mage

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
)

const vmName = "celeris-bench"

var Default = All

// All runs lint, test, and build.
func All() {
	mg.SerialDeps(Lint, Test, Build)
}

// Lint runs golangci-lint.
func Lint() error {
	return sh.RunV("golangci-lint", "run", "./...")
}

// Test runs all tests with the race detector.
func Test() error {
	return sh.RunV("go", "test", "-race", "-count=1", "./...")
}

// Build compiles all packages.
func Build() error {
	return sh.RunV("go", "build", "./...")
}

// Bench runs all benchmarks.
func Bench() error {
	return sh.RunV("go", "test", "-bench=.", "-benchmem", "-run=^$", "./...")
}

// Fuzz runs fuzz tests for the specified duration (default 30s).
func Fuzz() error {
	duration := "30s"
	if d := os.Getenv("FUZZ_TIME"); d != "" {
		duration = d
	}
	if err := sh.RunV("go", "test", "-fuzz=FuzzParseRequest", "-fuzztime="+duration, "./protocol/h1/"); err != nil {
		return err
	}
	return sh.RunV("go", "test", "-fuzz=FuzzParseChunkedBody", "-fuzztime="+duration, "./protocol/h1/")
}

// Clean removes build artifacts.
func Clean() error {
	return sh.RunV("go", "clean", "./...")
}

// Tools installs external test tools (h2spec).
func Tools() error {
	if _, err := exec.LookPath("h2spec"); err == nil {
		fmt.Println("h2spec: already installed")
		return nil
	}
	fmt.Println("Installing h2spec v2.6.0...")

	goos := runtime.GOOS
	// h2spec only ships amd64 binaries; Rosetta handles arm64 on macOS
	goarch := "amd64"

	if goos != "linux" && goos != "darwin" {
		fmt.Println("No prebuilt h2spec binary; trying go install...")
		return sh.RunV("go", "install", "github.com/summerwind/h2spec/cmd/h2spec@latest")
	}

	gopath, err := sh.Output("go", "env", "GOPATH")
	if err != nil {
		return fmt.Errorf("GOPATH: %w", err)
	}
	binDir := filepath.Join(gopath, "bin")

	tarball := fmt.Sprintf("h2spec_%s_%s.tar.gz", goos, goarch)
	url := fmt.Sprintf("https://github.com/summerwind/h2spec/releases/download/v2.6.0/%s", tarball)

	if err := sh.RunV("bash", "-c",
		fmt.Sprintf("curl -fsSL '%s' | tar xz -C '%s' h2spec", url, binDir)); err != nil {
		fmt.Println("Binary download failed; trying go install...")
		return sh.RunV("go", "install", "github.com/summerwind/h2spec/cmd/h2spec@latest")
	}
	return nil
}

// H2Spec runs HTTP/2 conformance tests using h2spec across all engines.
func H2Spec() error {
	return sh.RunV("go", "test", "-v", "-run", "TestH2Spec", "-count=1", "-timeout=120s", "./test/spec/...")
}

// H1Spec runs HTTP/1.1 RFC 9112 compliance tests across all engines.
func H1Spec() error {
	return sh.RunV("go", "test", "-v", "-run", "TestH1Spec", "-count=1", "-timeout=120s", "./test/spec/...")
}

// Spec runs all protocol compliance tests (h2spec + h1spec) across all engines.
func Spec() error {
	return sh.RunV("go", "test", "-v", "-count=1", "-timeout=120s", "./test/spec/...")
}

// LintLinux runs golangci-lint for Linux cross-compilation.
func LintLinux() error {
	env := map[string]string{"GOOS": "linux", "GOARCH": "amd64"}
	return sh.RunWithV(env, "golangci-lint", "run", "./...")
}

// Check runs the full verification suite: lint, tests, spec compliance, and build.
func Check() {
	mg.SerialDeps(Lint, Test, Spec, Build)
}

// goVersion reads the Go version from go.mod (e.g. "1.26.0" -> "1.26.0").
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

// vmArch returns the architecture string for the Go download URL
// based on the host machine (the VM inherits the host arch on Apple Silicon / x86).
func vmArch() string {
	arch := runtime.GOARCH
	if arch == "arm64" {
		return "arm64"
	}
	return "amd64"
}

// installGoScript returns a shell script that downloads and installs Go
// from the official go.dev tarball, matching the version in go.mod.
func installGoScript(goVer, arch string) string {
	tarball := fmt.Sprintf("go%s.linux-%s.tar.gz", goVer, arch)
	url := fmt.Sprintf("https://go.dev/dl/%s", tarball)
	return strings.Join([]string{
		"set -e",
		"cd /tmp",
		fmt.Sprintf("curl -fsSL -o /tmp/%s %s", tarball, url),
		fmt.Sprintf("sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf /tmp/%s", tarball),
		fmt.Sprintf("rm /tmp/%s", tarball),
		// Ensure go is on PATH for this and future sessions
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
		if err := sh.RunV("multipass", "launch", "--name", vmName,
			"--cpus", "4", "--memory", "4G", "--disk", "20G", "noble"); err != nil {
			return fmt.Errorf("failed to create VM: %w", err)
		}
		fmt.Println("Installing Go from go.dev...")
		script := installGoScript(goVer, arch)
		if err := sh.RunV("multipass", "exec", vmName, "--", "bash", "-c", script); err != nil {
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
	// Unmount any stale SSHFS mounts (they cause go.mod permission issues).
	_ = sh.RunV("multipass", "umount", vmName+":celeris")
	// Remove old copy and transfer fresh source.
	_ = sh.RunV("multipass", "exec", vmName, "--", "rm", "-rf", vmProjectDir)
	if err := sh.RunV("multipass", "transfer", "-r", cwd, vmName+":"+vmProjectDir); err != nil {
		return fmt.Errorf("failed to sync source: %w", err)
	}
	return nil
}

// BenchLinux runs benchmarks inside a Multipass VM for Linux-accurate results.
// Requires multipass to be installed (brew install multipass).
// Go is installed from the official go.dev tarball matching the version in go.mod.
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
	return sh.RunV("multipass", "exec", vmName, "--", "bash", "-c",
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
	return sh.RunV("multipass", "exec", vmName, "--", "bash", "-c",
		"export PATH=$PATH:/usr/local/go/bin && cd "+vmProjectDir+" && go test -race -count=1 ./...")
}

// SpecLinux runs the full h2spec + h1spec compliance suite inside a Multipass Linux VM
// where io_uring, epoll, and std engines are all available.
func SpecLinux() error {
	if runtime.GOOS == "linux" {
		fmt.Println("Already on Linux, running spec directly.")
		mg.Deps(Tools)
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
	return sh.RunV("multipass", "exec", vmName, "--", "bash", "-c",
		"export PATH=$PATH:/usr/local/go/bin:/usr/local/bin && cd "+vmProjectDir+" && "+
			"go test -v -count=1 -timeout=300s ./test/spec/...")
}

// CheckLinux runs the full verification suite (lint, test, spec, build) inside a Linux VM.
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
	return sh.RunV("multipass", "exec", vmName, "--", "bash", "-c",
		"export PATH=$PATH:/usr/local/go/bin:/usr/local/bin && cd "+vmProjectDir+" && "+
			"go test -race -count=1 ./... && "+
			"go test -v -count=1 -timeout=300s ./test/spec/...")
}

// VMStop stops the Multipass benchmark VM.
func VMStop() error {
	return sh.RunV("multipass", "stop", vmName)
}

// VMDelete deletes the Multipass benchmark VM.
func VMDelete() error {
	if err := sh.RunV("multipass", "delete", vmName); err != nil {
		return err
	}
	return sh.RunV("multipass", "purge")
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
	// Check if h2spec already exists
	if err := exec.Command("multipass", "exec", vmName, "--", "which", "h2spec").Run(); err == nil {
		fmt.Println("h2spec: already installed in VM")
		return nil
	}
	fmt.Println("Installing h2spec in VM...")
	arch := vmArch()
	if arch == "arm64" {
		// h2spec only provides amd64 binaries; build from source on arm64.
		// Work from /tmp to avoid picking up the mounted celeris go.mod.
		return sh.RunV("multipass", "exec", vmName, "--", "bash", "-c",
			"cd /tmp && export GOPATH=/tmp/gopath && mkdir -p $GOPATH && "+
				"/usr/local/go/bin/go install github.com/summerwind/h2spec/cmd/h2spec@latest && "+
				"sudo cp /tmp/gopath/bin/h2spec /usr/local/bin/h2spec")
	}
	tarball := fmt.Sprintf("h2spec_linux_%s.tar.gz", arch)
	url := fmt.Sprintf("https://github.com/summerwind/h2spec/releases/download/v2.6.0/%s", tarball)
	return sh.RunV("multipass", "exec", vmName, "--", "bash", "-c",
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
				return sh.RunV("multipass", "start", name)
			}
			return nil
		}
	}
	return fmt.Errorf("VM %s not found", name)
}
