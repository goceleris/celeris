// Command runner is the perfmatrix orchestrator binary. Wave-3 wires it
// into the interleave scheduler, service lifecycle, report aggregator,
// and optional pprof capture. Today it only parses and echoes its flags
// so every target-script / mage wrapper can be wired end-to-end.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// Config is the parsed flag set. Exported for cmd/runner tests.
type Config struct {
	Runs      int
	Duration  time.Duration
	Warmup    time.Duration
	Cells     string
	Out       string
	Profile   bool
	Services  string
	MagePhase string
}

// DefaultConfig is the set of defaults applied to a fresh flag.FlagSet.
func DefaultConfig() Config {
	return Config{
		Runs:      10,
		Duration:  10 * time.Second,
		Warmup:    2 * time.Second,
		Cells:     "",
		Out:       "",
		Profile:   false,
		Services:  "local",
		MagePhase: "full",
	}
}

// Bind registers every Config field onto fs. Separated from ParseArgs so
// unit tests can drive parsing deterministically.
func (c *Config) Bind(fs *flag.FlagSet) {
	fs.IntVar(&c.Runs, "runs", c.Runs,
		"number of interleaved passes through the matrix")
	fs.DurationVar(&c.Duration, "duration", c.Duration,
		"measurement window per cell")
	fs.DurationVar(&c.Warmup, "warmup", c.Warmup,
		"pre-measurement warmup per cell")
	fs.StringVar(&c.Cells, "cells", c.Cells,
		`glob filter over "<scenario>/<server>" (e.g. "celeris-*/get-json", "*/driver-*")`)
	fs.StringVar(&c.Out, "out", c.Out,
		"output directory; default results/<timestamp>-<git-ref>/")
	fs.BoolVar(&c.Profile, "profile", c.Profile,
		"enable pprof capture per cell")
	fs.StringVar(&c.Services, "services", c.Services,
		`"local" | "msr1" | "none"`)
	fs.StringVar(&c.MagePhase, "mage-phase", c.MagePhase,
		`internal; "full" | "quick" | "drivers" | "since"`)
}

// ParseArgs parses argv (without the program name). out is used for flag
// usage / error output; typically os.Stderr.
func ParseArgs(args []string, out io.Writer) (Config, error) {
	cfg := DefaultConfig()
	fs := flag.NewFlagSet("perfmatrix-runner", flag.ContinueOnError)
	fs.SetOutput(out)
	cfg.Bind(fs)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func main() {
	cfg, err := ParseArgs(os.Args[1:], os.Stderr)
	if err != nil {
		os.Exit(2)
	}
	fmt.Printf("perfmatrix runner scaffold — args parsed: %+v\n", cfg)
	fmt.Println("wave-3 agent wires the orchestrator body")
}
