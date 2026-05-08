package main

import (
	"bytes"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.Runs != 10 {
		t.Errorf("Runs = %d, want 10", c.Runs)
	}
	if c.Duration != 10*time.Second {
		t.Errorf("Duration = %s, want 10s", c.Duration)
	}
	if c.Warmup != 2*time.Second {
		t.Errorf("Warmup = %s, want 2s", c.Warmup)
	}
	if c.Services != "local" {
		t.Errorf("Services = %q, want local", c.Services)
	}
	if c.MagePhase != "full" {
		t.Errorf("MagePhase = %q, want full", c.MagePhase)
	}
	if c.Profile {
		t.Errorf("Profile = true, want false")
	}
}

func TestParseArgsOverrides(t *testing.T) {
	var buf bytes.Buffer
	cfg, err := ParseArgs([]string{
		"-runs", "3",
		"-duration", "5s",
		"-warmup", "1s",
		"-cells", "celeris-*/get-json",
		"-out", "/tmp/pm",
		"-profile",
		"-services", "msr1",
		"-mage-phase", "quick",
	}, &buf)
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	if cfg.Runs != 3 {
		t.Errorf("Runs = %d, want 3", cfg.Runs)
	}
	if cfg.Duration != 5*time.Second {
		t.Errorf("Duration = %s, want 5s", cfg.Duration)
	}
	if cfg.Warmup != time.Second {
		t.Errorf("Warmup = %s, want 1s", cfg.Warmup)
	}
	if cfg.Cells != "celeris-*/get-json" {
		t.Errorf("Cells = %q", cfg.Cells)
	}
	if cfg.Out != "/tmp/pm" {
		t.Errorf("Out = %q", cfg.Out)
	}
	if !cfg.Profile {
		t.Errorf("Profile = false, want true")
	}
	if cfg.Services != "msr1" {
		t.Errorf("Services = %q", cfg.Services)
	}
	if cfg.MagePhase != "quick" {
		t.Errorf("MagePhase = %q", cfg.MagePhase)
	}
}

func TestParseArgsInvalid(t *testing.T) {
	var buf bytes.Buffer
	if _, err := ParseArgs([]string{"-runs", "notanumber"}, &buf); err == nil {
		t.Fatalf("expected error for non-integer -runs")
	}
}

func TestParseArgsUnknownFlag(t *testing.T) {
	var buf bytes.Buffer
	if _, err := ParseArgs([]string{"-nope"}, &buf); err == nil {
		t.Fatalf("expected error for unknown flag")
	}
}

// TestCellTimeoutFormula pins the per-cell timeout schedule. The
// formula is load-bearing: too-short timeouts produced spurious
// 0-request failures under -race on slow hosts (msa2-server,
// msr1 with checkptr=2). Locking it down means a future "let's
// halve the timeout because cells finish fast on this host" change
// gets caught in CI.
func TestCellTimeoutFormula(t *testing.T) {
	for _, tc := range []struct {
		name     string
		warmup   time.Duration
		duration time.Duration
		want     time.Duration
	}{
		{"defaults", 2 * time.Second, 10 * time.Second, 5*2*time.Second + 2*10*time.Second + 60*time.Second},
		{"zero", 0, 0, 60 * time.Second},
		{"long", 10 * time.Second, 60 * time.Second, 50*time.Second + 120*time.Second + 60*time.Second},
		{"short", 250 * time.Millisecond, time.Second, 1250*time.Millisecond + 2*time.Second + 60*time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cellTimeoutFor(tc.warmup, tc.duration); got != tc.want {
				t.Errorf("cellTimeoutFor(%v, %v) = %v, want %v", tc.warmup, tc.duration, got, tc.want)
			}
		})
	}
}

// TestCountProcessFDs sanity-checks the leak-diag helper. On Linux
// (the bench hosts where it matters) at least the test process's own
// stdin/stdout/stderr exist, so the count is positive. On macOS it
// silently returns 0 — the helper is best-effort. Both behaviours
// are valid; just ensure no panic and a non-negative count.
func TestCountProcessFDs(t *testing.T) {
	got := countProcessFDs()
	if got < 0 {
		t.Errorf("countProcessFDs() = %d, want >= 0", got)
	}
	// On Linux the test binary inherits at least 3 FDs (stdin, stdout,
	// stderr) plus the /proc/self/fd directory handle itself, so > 0.
	// On macOS /proc is absent → returns 0; don't over-pin.
}
