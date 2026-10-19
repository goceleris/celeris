package probe

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/goceleris/celeris/internal/engine"
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

// TestDiagnosticNotesTheEngineFloor (checklist #884 item 13): the capability
// profile reports io_uring_tier=base, and the io_uring feature block, on a
// 5.10 to 5.18 kernel, where the io_uring ENGINE refuses to start (it needs
// the IORING_ASYNC_CANCEL flags of Linux 5.19, celeris#682). Both renderings
// say so on such a kernel, and say nothing on one at or above the floor or on
// a host without io_uring. Neither function has a production caller yet
// (doc.go offers them to an application), so this is the only place the
// wording is exercised.
func TestDiagnosticNotesTheEngineFloor(t *testing.T) {
	const note = "needs Linux 5.19"
	// The epoll fallback is the adaptive engine's alone: an explicit
	// Engine: IOUring fails to start (review round 1 of this change), so the
	// note must say both and not claim that epoll serves.
	const explicit, wrong = "explicit io_uring engine fails", "epoll engine serves instead"
	for _, tc := range []struct {
		name         string
		version      string
		major, minor int
		tier         engine.Tier
		want         bool
	}{
		{"5.10-base", "5.10.0", 5, 10, engine.Base, true},
		{"5.15-base", "5.15.0-191-generic", 5, 15, engine.Base, true},
		{"5.18-base", "5.18.19", 5, 18, engine.Base, true},
		{"5.19-high", "5.19.0", 5, 19, engine.High, false},
		// A 5.19+ kernel the probe holds at Base (a suspect vendor kernel,
		// probe/tier.go) is at the floor: the note is about the version.
		{"5.19-base", "5.19.0", 5, 19, engine.Base, false},
		{"6.1-optional", "6.1.0", 6, 1, engine.Optional, false},
		{"4.19-none", "4.19.0", 4, 19, engine.None, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := engine.CapabilityProfile{
				OS: "linux", KernelVersion: tc.version, KernelMajor: tc.major, KernelMinor: tc.minor,
				IOUringTier: tc.tier, EpollAvailable: true, NumCPU: 4, NUMANodes: 1,
			}
			if got := strings.Contains(FormatDiagnostic(profile), note); got != tc.want {
				t.Errorf("FormatDiagnostic mentions %q = %v, want %v:\n%s", note, got, tc.want, FormatDiagnostic(profile))
			}
			var buf bytes.Buffer
			DiagnosticReport(profile, slog.New(slog.NewTextHandler(&buf, nil)))
			if got := strings.Contains(buf.String(), note); got != tc.want {
				t.Errorf("DiagnosticReport mentions %q = %v, want %v:\n%s", note, got, tc.want, buf.String())
			}
			if tc.want {
				for name, out := range map[string]string{"FormatDiagnostic": FormatDiagnostic(profile), "DiagnosticReport": buf.String()} {
					if !strings.Contains(out, explicit) || strings.Contains(out, wrong) {
						t.Errorf("%s must say an %q and not %q:\n%s", name, explicit, wrong, out)
					}
				}
			}
		})
	}
}
