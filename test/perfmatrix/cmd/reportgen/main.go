// Command reportgen reduces a perfmatrix output directory into the
// human-consumable report set: report.md, aggregated.csv, and (when
// pprof captures are present) profiles.html.
//
// Usage:
//
//	reportgen -in results/<ts>-matrix-<ref>
//
// The -in directory is produced by cmd/runner; reportgen walks every
// runN/<scenario>/<server>.json file and groups samples by
// (scenario, server) before feeding them to [report.Aggregate].
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/goceleris/loadgen"

	"github.com/goceleris/celeris/test/perfmatrix/report"
)

type cellResultFile struct {
	RunIdx       int             `json:"run_idx"`
	ScenarioName string          `json:"scenario"`
	ServerName   string          `json:"server"`
	ServerKind   string          `json:"server_kind"`
	Category     string          `json:"category"`
	StartedAt    time.Time       `json:"started_at"`
	CompletedAt  time.Time       `json:"completed_at"`
	Error        string          `json:"error,omitempty"`
	Result       *loadgen.Result `json:"result,omitempty"`
	Profile      json.RawMessage `json:"profile,omitempty"`
}

type manifest struct {
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	GitSHA      string    `json:"git_sha,omitempty"`
	LoadgenVer  string    `json:"loadgen_version,omitempty"`
	CellCount   int       `json:"cell_count"`
	Config      struct {
		Runs     int           `json:"runs"`
		Duration time.Duration `json:"duration"`
	} `json:"config"`
	Host struct {
		OS       string `json:"os"`
		Arch     string `json:"arch"`
		Hostname string `json:"hostname,omitempty"`
	} `json:"host"`
}

func main() {
	in := flag.String("in", "", "perfmatrix output directory (mandatory)")
	flag.Parse()
	if *in == "" {
		fmt.Fprintln(os.Stderr, "reportgen: -in is required")
		os.Exit(2)
	}
	if err := generate(*in); err != nil {
		fmt.Fprintf(os.Stderr, "reportgen: %v\n", err)
		os.Exit(1)
	}
}

func generate(dir string) error {
	// Aggregate per-cell JSONs by (scenario, server).
	type key struct{ scenario, server string }
	type bucket struct {
		serverKind string
		category   string
		samples    []loadgen.Result
	}
	buckets := map[key]*bucket{}

	var anyProfile bool
	walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".json") {
			return nil
		}
		base := filepath.Base(path)
		if base == "manifest.json" {
			return nil
		}
		data, rErr := os.ReadFile(path)
		if rErr != nil {
			return nil
		}
		var cm cellResultFile
		if err := json.Unmarshal(data, &cm); err != nil || cm.Result == nil {
			return nil
		}
		if len(cm.Profile) > 0 && string(cm.Profile) != "null" {
			anyProfile = true
		}
		k := key{cm.ScenarioName, cm.ServerName}
		b, ok := buckets[k]
		if !ok {
			b = &bucket{serverKind: cm.ServerKind, category: cm.Category}
			buckets[k] = b
		}
		b.samples = append(b.samples, *cm.Result)
		return nil
	})
	if walkErr != nil {
		return walkErr
	}

	// Stable iteration order makes diffing trivial.
	keys := make([]key, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].scenario != keys[j].scenario {
			return keys[i].scenario < keys[j].scenario
		}
		return keys[i].server < keys[j].server
	})

	cells := make([]report.CellResult, 0, len(keys))
	for _, k := range keys {
		b := buckets[k]
		cells = append(cells, report.CellResult{
			ScenarioName: k.scenario,
			ServerName:   k.server,
			ServerKind:   b.serverKind,
			Category:     b.category,
			Samples:      b.samples,
		})
	}

	agg := report.Aggregate(cells)

	// Load manifest for Meta. Not fatal if missing; the runner always
	// writes it, but a partial/crashed run may lack it.
	var mf manifest
	if data, err := os.ReadFile(filepath.Join(dir, "manifest.json")); err == nil {
		_ = json.Unmarshal(data, &mf)
	}
	meta := report.Meta{
		GitRef:     mf.GitSHA,
		StartedAt:  mf.StartedAt,
		FinishedAt: mf.CompletedAt,
		Host:       strings.TrimSpace(mf.Host.Hostname + " " + mf.Host.OS + "/" + mf.Host.Arch),
		LoadgenVer: mf.LoadgenVer,
		Runs:       mf.Config.Runs,
		Duration:   mf.Config.Duration,
		TotalCells: len(agg),
	}
	if meta.Host == "/" {
		// Fallback when manifest lacks host info.
		meta.Host = runtime.GOOS + "/" + runtime.GOARCH
	}

	// Write report.md.
	mdPath := filepath.Join(dir, "report.md")
	md, err := os.Create(mdPath)
	if err != nil {
		return err
	}
	if err := report.WriteMarkdown(md, agg, meta); err != nil {
		_ = md.Close()
		return err
	}
	if err := md.Close(); err != nil {
		return err
	}

	// Write aggregated.csv.
	csvPath := filepath.Join(dir, "aggregated.csv")
	csvFile, err := os.Create(csvPath)
	if err != nil {
		return err
	}
	if err := report.WriteCSV(csvFile, agg); err != nil {
		_ = csvFile.Close()
		return err
	}
	if err := csvFile.Close(); err != nil {
		return err
	}

	// Write profiles.html when profile artefacts exist.
	if anyProfile {
		htmlPath := filepath.Join(dir, "profiles.html")
		html, err := os.Create(htmlPath)
		if err != nil {
			return err
		}
		if err := report.WritePprofIndex(html, dir); err != nil {
			_ = html.Close()
			return err
		}
		if err := html.Close(); err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "reportgen: wrote %s and %s\n", mdPath, csvPath)
	return nil
}
