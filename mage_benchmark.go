//go:build mage

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const benchmarkVM = "celeris-benchmark"

// LocalBenchmark runs interleaved A/B benchmarks comparing the current branch
// against main. On non-Linux hosts, a Multipass VM is created and destroyed.
// Results are saved to results/<timestamp>-local-benchmark/.
func LocalBenchmark() error {
	branch, err := currentBranch()
	if err != nil {
		return err
	}
	dir, err := resultsDir("local-benchmark")
	if err != nil {
		return err
	}
	arch := hostArch()

	fmt.Printf("A/B Benchmark: %s vs main (linux/%s)\n\n", branch, arch)

	// Cross-compile fullstack-loadtest for both branches.
	currentBin := filepath.Join(dir, "fullstack-current")
	mainBin := filepath.Join(dir, "fullstack-main")

	fmt.Println("Building binaries...")
	if err := crossCompile("test/loadtest/fullstack", currentBin, arch); err != nil {
		return fmt.Errorf("build current branch: %w", err)
	}
	if err := crossCompileFromRef("main", "test/loadtest/fullstack", mainBin, arch); err != nil {
		return fmt.Errorf("build main branch: %w", err)
	}

	var mainRuns, currentRuns [][]ConfigResult

	if runtime.GOOS == "linux" {
		mainRuns, currentRuns, err = runBenchmarkDirect(mainBin, currentBin)
	} else {
		mainRuns, currentRuns, err = runBenchmarkInVM(dir, mainBin, currentBin, arch)
	}
	if err != nil {
		return err
	}

	report := generateABReport(branch, mainRuns, currentRuns)
	fmt.Println("\n" + report)

	reportPath := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(reportPath, []byte(report), 0o644); err != nil {
		return err
	}
	fmt.Printf("Report saved to: %s\n", reportPath)
	return nil
}

func runBenchmarkDirect(mainBin, currentBin string) ([][]ConfigResult, [][]ConfigResult, error) {
	fmt.Println("Running benchmarks directly on Linux...")
	execFn := func(binary string) (string, error) {
		cmd := exec.Command("sudo", "prlimit", "--memlock=unlimited", binary)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	return runInterleaved(execFn, mainBin, currentBin)
}

func runBenchmarkInVM(dir, mainBin, currentBin, arch string) ([][]ConfigResult, [][]ConfigResult, error) {
	fmt.Println("Not on Linux — running benchmarks in Multipass VM.")

	if err := ensureVMWithSpecs(benchmarkVM, "4", "8G"); err != nil {
		return nil, nil, err
	}
	defer destroyVM(benchmarkVM)

	// Transfer binaries to VM.
	if err := transferToVM(benchmarkVM, mainBin, "/tmp/bench/fullstack-main"); err != nil {
		return nil, nil, fmt.Errorf("transfer main binary: %w", err)
	}
	if err := transferToVM(benchmarkVM, currentBin, "/tmp/bench/fullstack-current"); err != nil {
		return nil, nil, fmt.Errorf("transfer current binary: %w", err)
	}
	// Make executable.
	_, _ = vmExec(benchmarkVM, "chmod +x /tmp/bench/fullstack-main /tmp/bench/fullstack-current")

	execFn := func(binary string) (string, error) {
		remoteBin := "/tmp/bench/fullstack-main"
		if binary == currentBin {
			remoteBin = "/tmp/bench/fullstack-current"
		}
		return vmExec(benchmarkVM,
			fmt.Sprintf("sudo prlimit --memlock=unlimited %s", remoteBin))
	}

	return runInterleaved(execFn, mainBin, currentBin)
}

// CloudBenchmark runs interleaved A/B benchmarks on AWS EC2 instances.
// Set CLOUD_ARCH=amd64|arm64|both (default: both).
// Instances are auto-created and terminated after benchmarks complete.
// Results are saved to results/<timestamp>-cloud-benchmark/.
func CloudBenchmark() error {
	if err := awsEnsureCLI(); err != nil {
		return err
	}

	branch, err := currentBranch()
	if err != nil {
		return err
	}
	dir, err := resultsDir("cloud-benchmark")
	if err != nil {
		return err
	}
	arches := cloudArch()

	fmt.Printf("Cloud A/B Benchmark: %s vs main (%v)\n\n", branch, arches)

	// Create shared AWS resources.
	keyName, keyPath, err := awsCreateKeyPair(dir)
	if err != nil {
		return err
	}
	sgID, err := awsCreateSecurityGroup()
	if err != nil {
		awsDeleteKeyPair(keyName)
		return err
	}

	var allInstanceIDs []string
	defer func() {
		awsCleanup(allInstanceIDs, keyName, sgID, keyPath)
	}()

	for _, arch := range arches {
		fmt.Printf("\n====== Architecture: %s ======\n\n", arch)

		// Cross-compile for this architecture.
		currentBin := filepath.Join(dir, fmt.Sprintf("fullstack-current-%s", arch))
		mainBin := filepath.Join(dir, fmt.Sprintf("fullstack-main-%s", arch))

		fmt.Println("Building binaries...")
		if err := crossCompile("test/loadtest/fullstack", currentBin, arch); err != nil {
			return fmt.Errorf("build current (%s): %w", arch, err)
		}
		if err := crossCompileFromRef("main", "test/loadtest/fullstack", mainBin, arch); err != nil {
			return fmt.Errorf("build main (%s): %w", arch, err)
		}

		// Launch instance.
		ami, err := awsLatestAMI(arch)
		if err != nil {
			return err
		}
		instanceID, ip, err := awsLaunchInstance(ami, awsInstanceType(arch), keyName, sgID, arch)
		if err != nil {
			return err
		}
		allInstanceIDs = append(allInstanceIDs, instanceID)

		if err := awsWaitSSH(ip, keyPath); err != nil {
			return err
		}
		if err := awsInstallGo(ip, keyPath); err != nil {
			return fmt.Errorf("install Go on %s: %w", arch, err)
		}

		// Transfer binaries.
		fmt.Println("Transferring binaries...")
		if _, err := awsSSH(ip, keyPath, "mkdir -p /tmp/bench"); err != nil {
			return err
		}
		if err := awsSCP(mainBin, "/tmp/bench/fullstack-main", ip, keyPath); err != nil {
			return err
		}
		if err := awsSCP(currentBin, "/tmp/bench/fullstack-current", ip, keyPath); err != nil {
			return err
		}
		if _, err := awsSSH(ip, keyPath, "chmod +x /tmp/bench/fullstack-*"); err != nil {
			return err
		}

		// Run interleaved benchmarks.
		execFn := func(binary string) (string, error) {
			remoteBin := "/tmp/bench/fullstack-main"
			if binary == currentBin {
				remoteBin = "/tmp/bench/fullstack-current"
			}
			return awsSSH(ip, keyPath,
				fmt.Sprintf("sudo prlimit --memlock=unlimited %s", remoteBin))
		}

		mainRuns, currentRuns, err := runInterleaved(execFn, mainBin, currentBin)
		if err != nil {
			return err
		}

		report := generateABReport(branch+" ("+arch+")", mainRuns, currentRuns)
		fmt.Println("\n" + report)

		reportPath := filepath.Join(dir, fmt.Sprintf("report-%s.txt", arch))
		_ = os.WriteFile(reportPath, []byte(report), 0o644)
		fmt.Printf("Report saved to: %s\n", reportPath)
	}

	return nil
}
