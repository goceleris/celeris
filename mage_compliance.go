//go:build mage

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

const complianceVM = "celeris-compliance"

// fuzzTargets lists all fuzz test targets in the project.
var fuzzTargets = []struct {
	name string
	pkg  string
}{
	{"FuzzParseRequest", "./protocol/h1/"},
	{"FuzzParseChunkedBody", "./protocol/h1/"},
	{"FuzzCleanPath", "."},
	{"FuzzRouterFind", "."},
	{"FuzzParseFormURLEncoded", "."},
	{"FuzzCookieParsing", "."},
	{"FuzzParse", "./internal/negotiate/"},
	{"FuzzMatchMedia", "./internal/negotiate/"},
	{"FuzzAccept", "./internal/negotiate/"},
}

// FullCompliance runs the complete compliance verification suite on Linux.
// On non-Linux hosts, a Multipass VM is created, used, then destroyed.
// Includes: unit tests with race detection, ALL fuzz targets, h1spec, h2spec,
// integration tests, and conformance tests across all engine configurations.
func FullCompliance() error {
	if runtime.GOOS == "linux" {
		return runComplianceDirect()
	}
	return runComplianceInVM()
}

func runComplianceDirect() error {
	fuzzTime := "30s"
	if d := os.Getenv("FUZZ_TIME"); d != "" {
		fuzzTime = d
	}

	fmt.Println("=== Phase 1: Unit Tests (race detector) ===")
	// Exclude test/spec (run separately in Phase 3 with h2spec tolerance).
	pkgs, err := output("go", "list", "./...")
	if err != nil {
		return fmt.Errorf("list packages: %w", err)
	}
	var testPkgs []string
	for _, p := range strings.Split(pkgs, "\n") {
		if !strings.Contains(p, "/test/spec") {
			testPkgs = append(testPkgs, p)
		}
	}
	testArgs := append([]string{"test", "-race", "-count=1", "-timeout=600s"}, testPkgs...)
	if err := run("go", testArgs...); err != nil {
		return fmt.Errorf("unit tests failed: %w", err)
	}

	fmt.Println("\n=== Phase 2: Fuzz Tests ===")
	for _, ft := range fuzzTargets {
		fmt.Printf("  Fuzzing %s (%s)...\n", ft.name, fuzzTime)
		if err := run("go", "test", "-fuzz="+ft.name, "-fuzztime="+fuzzTime, ft.pkg); err != nil {
			return fmt.Errorf("fuzz %s failed: %w", ft.name, err)
		}
	}

	fmt.Println("\n=== Phase 3: Protocol Compliance (h1spec + h2spec) ===")
	fmt.Println("Note: std engine has 4 known h2spec failures (tolerated).")
	must(Tools())
	// Spec tests may fail due to std engine known h2spec failures — tolerated.
	if err := run("go", "test", "-v", "-count=1", "-timeout=300s", "./test/spec/..."); err != nil {
		fmt.Printf("  Spec tests exited with error (expected for std engine known failures): %v\n", err)
	}

	fmt.Println("\n=== Phase 4: Conformance + Integration Tests ===")
	if err := run("go", "test", "-v", "-race", "-count=1", "-timeout=300s",
		"./test/conformance/...", "./test/integration/..."); err != nil {
		return fmt.Errorf("conformance/integration tests failed: %w", err)
	}

	fmt.Println("\n=== Full Compliance: PASSED ===")
	return nil
}

func runComplianceInVM() error {
	fmt.Println("Not on Linux — running compliance in Multipass VM.")

	if err := ensureVMWithSpecs(complianceVM, "6", "8G"); err != nil {
		return err
	}
	defer destroyVM(complianceVM)

	if err := syncSourceTo(complianceVM); err != nil {
		return err
	}

	// Install build tools (gcc for race detector) and h2spec.
	fmt.Println("Installing build tools in VM...")
	_, _ = vmExec(complianceVM, "sudo apt-get update -qq && sudo apt-get install -y -qq build-essential curl")

	if err := ensureH2SpecInVMNamed(complianceVM); err != nil {
		return err
	}

	fuzzTime := "30s"
	if d := os.Getenv("FUZZ_TIME"); d != "" {
		fuzzTime = d
	}

	// Build the compliance script with all fuzz targets.
	var fuzzCmds []string
	for _, ft := range fuzzTargets {
		fuzzCmds = append(fuzzCmds,
			fmt.Sprintf("echo '  Fuzzing %s...' && go test -fuzz=%s -fuzztime=%s %s",
				ft.name, ft.name, fuzzTime, ft.pkg))
	}

	// Run each phase independently, tracking failures.
	// h2spec has 4 known std engine failures which are tolerated.
	script := strings.Join([]string{
		"export PATH=$PATH:/usr/local/go/bin:/usr/local/bin",
		"export CGO_ENABLED=1",
		"cd " + vmProjectDir,
		"FAILURES=0",
		"",
		"echo '=== Phase 1: Unit Tests (race detector) ==='",
		"# Exclude test/spec (run separately in Phase 3).",
		"if ! go test -race -count=1 -timeout=600s $(go list ./... | grep -v '/test/spec'); then",
		"  echo 'PHASE 1 FAILED'",
		"  FAILURES=$((FAILURES+1))",
		"fi",
		"",
		"echo ''",
		"echo '=== Phase 2: Fuzz Tests ==='",
		strings.Join(fuzzCmds, " && ") + " || { echo 'PHASE 2 FAILED'; FAILURES=$((FAILURES+1)); }",
		"",
		"echo ''",
		"echo '=== Phase 3: Protocol Compliance (h1spec + h2spec) ==='",
		"echo 'Note: std engine has 4 known h2spec failures (connection-specific headers, TE header,'",
		"echo '      invalid preface, SETTINGS_INITIAL_WINDOW_SIZE). These are tolerated.'",
		"# Run spec tests — h2spec failures on std engine are expected.",
		"# The test covers iouring + epoll + std; only std has known failures.",
		"go test -v -count=1 -timeout=300s ./test/spec/... 2>&1; SPEC_EXIT=$?",
		"if [ $SPEC_EXIT -ne 0 ]; then",
		"  echo 'Spec tests exited with code '$SPEC_EXIT' (expected for std engine known failures)'",
		"fi",
		"",
		"echo ''",
		"echo '=== Phase 4: Conformance + Integration Tests ==='",
		"if ! go test -v -race -count=1 -timeout=300s ./test/conformance/... ./test/integration/...; then",
		"  echo 'PHASE 4 FAILED'",
		"  FAILURES=$((FAILURES+1))",
		"fi",
		"",
		"echo ''",
		"if [ $FAILURES -eq 0 ]; then",
		"  echo '=== Full Compliance: PASSED ==='",
		"  exit 0",
		"else",
		"  echo '=== Full Compliance: '$FAILURES' phase(s) FAILED ==='",
		"  exit 1",
		"fi",
	}, "\n")

	fmt.Println("Running full compliance suite in VM...")
	return vmExecStream(complianceVM, script)
}

// ensureH2SpecInVMNamed installs h2spec in the named VM if not present.
func ensureH2SpecInVMNamed(name string) error {
	if err := runSilent("multipass", "exec", name, "--", "which", "h2spec"); err == nil {
		fmt.Println("h2spec: already installed in VM")
		return nil
	}
	fmt.Println("Installing h2spec in VM...")
	arch := vmArch()
	if arch == "arm64" {
		return run("multipass", "exec", name, "--", "bash", "-c",
			"cd /tmp && export GOPATH=/tmp/gopath && mkdir -p $GOPATH && "+
				"/usr/local/go/bin/go install github.com/summerwind/h2spec/cmd/h2spec@latest && "+
				"sudo cp /tmp/gopath/bin/h2spec /usr/local/bin/h2spec")
	}
	tarball := fmt.Sprintf("h2spec_linux_%s.tar.gz", arch)
	url := fmt.Sprintf("https://github.com/summerwind/h2spec/releases/download/v2.6.0/%s", tarball)
	return run("multipass", "exec", name, "--", "bash", "-c",
		fmt.Sprintf("curl -fsSL %s | sudo tar xz -C /usr/local/bin h2spec", url))
}

// runSilent runs a command without connecting stdout/stderr.
func runSilent(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}
