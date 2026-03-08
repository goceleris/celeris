//go:build mage

package main

import (
	"fmt"
	"os"
	"os/exec"
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

func syncSource() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	fmt.Println("Syncing source to VM...")
	// Prefer mount for live editing; transfer as fallback
	_ = sh.RunV("multipass", "umount", vmName+":celeris")
	if err := sh.RunV("multipass", "mount", cwd, vmName+":celeris"); err != nil {
		// Mount may fail (e.g. --classic driver); fall back to copy
		if err := sh.RunV("multipass", "transfer", "-r", cwd, vmName+":celeris"); err != nil {
			return fmt.Errorf("failed to sync source: %w", err)
		}
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
		"export PATH=$PATH:/usr/local/go/bin && cd celeris && go test -bench=. -benchmem -run='^$' ./...")
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
		"export PATH=$PATH:/usr/local/go/bin && cd celeris && go test -race -count=1 ./...")
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
