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
