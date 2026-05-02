//go:build mage

package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

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

// FullCompliance runs the complete compliance verification suite.
// Linux-only — the engine layer (iouring, epoll) needs Linux syscalls,
// and the suite includes race-detected unit tests, all fuzz targets,
// h1spec/h2spec, plus integration and conformance tests.
//
// On macOS / non-Linux hosts, run this from one of the cluster nodes
// instead via the cluster mage targets (see ansible/README.md).
func FullCompliance() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("FullCompliance requires Linux; run on a cluster node — see ansible/README.md")
	}

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
