package probe

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/goceleris/celeris/engine"
)

func TestFormatDiagnostic(t *testing.T) {
	profile := engine.CapabilityProfile{
		OS:              "linux",
		KernelVersion:   "6.1.0-18-generic",
		KernelMajor:     6,
		KernelMinor:     1,
		IOUringTier:     engine.Optional,
		EpollAvailable:  true,
		MultishotAccept: true,
		MultishotRecv:   true,
		ProvidedBuffers: true,
		SQPoll:          true,
		CoopTaskrun:     true,
		SingleIssuer:    true,
		LinkedSQEs:      true,
		NumCPU:          8,
		NUMANodes:       2,
	}

	output := FormatDiagnostic(profile)

	expected := []string{
		"OS: linux",
		"Kernel: 6.1.0-18-generic (major=6, minor=1)",
		"io_uring Tier: optional",
		"epoll Available: true",
		"CPUs: 8",
		"NUMA Nodes: 2",
		"MultishotAccept: true",
		"MultishotRecv: true",
		"ProvidedBuffers: true",
		"SQPoll: true",
		"CoopTaskrun: true",
		"SingleIssuer: true",
		"LinkedSQEs: true",
	}

	for _, s := range expected {
		if !strings.Contains(output, s) {
			t.Errorf("output missing %q\ngot:\n%s", s, output)
		}
	}
}

func TestFormatDiagnosticNoIOUring(t *testing.T) {
	profile := engine.CapabilityProfile{
		OS:             "linux",
		KernelVersion:  "4.19.0",
		KernelMajor:    4,
		KernelMinor:    19,
		IOUringTier:    engine.None,
		EpollAvailable: true,
		NumCPU:         4,
		NUMANodes:      1,
	}

	output := FormatDiagnostic(profile)

	if strings.Contains(output, "MultishotAccept") {
		t.Error("output should not contain io_uring features when tier is None")
	}
	if !strings.Contains(output, "io_uring Tier: none") {
		t.Error("output should contain tier none")
	}
}

func TestDiagnosticReport(t *testing.T) {
	profile := engine.CapabilityProfile{
		OS:              "linux",
		KernelVersion:   "6.1.0",
		KernelMajor:     6,
		KernelMinor:     1,
		IOUringTier:     engine.Optional,
		EpollAvailable:  true,
		MultishotAccept: true,
		MultishotRecv:   true,
		ProvidedBuffers: true,
		SQPoll:          false,
		CoopTaskrun:     true,
		SingleIssuer:    true,
		LinkedSQEs:      true,
		NumCPU:          4,
		NUMANodes:       1,
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	DiagnosticReport(profile, logger)

	output := buf.String()
	if !strings.Contains(output, "capability probe complete") {
		t.Errorf("expected 'capability probe complete' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "io_uring features") {
		t.Errorf("expected 'io_uring features' in output, got:\n%s", output)
	}
}

func TestDiagnosticReportNoIOUring(t *testing.T) {
	profile := engine.CapabilityProfile{
		OS:             "darwin",
		IOUringTier:    engine.None,
		EpollAvailable: false,
		NumCPU:         4,
		NUMANodes:      1,
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	DiagnosticReport(profile, logger)

	output := buf.String()
	if !strings.Contains(output, "capability probe complete") {
		t.Errorf("expected 'capability probe complete' in output, got:\n%s", output)
	}
	if strings.Contains(output, "io_uring features") {
		t.Errorf("should not contain 'io_uring features' when tier is None, got:\n%s", output)
	}
}
