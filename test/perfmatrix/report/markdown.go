package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Meta is the ambient information emitted in the markdown preamble.
type Meta struct {
	GitRef     string
	StartedAt  time.Time
	FinishedAt time.Time
	Host       string
	LoadgenVer string
	CelerisVer string
	Runs       int
	Duration   time.Duration
	TotalCells int
}

// WriteMarkdown renders the top-level report.md. Sections are keyed off
// the scenario Category so the same code-path serves every category.
// Within each section servers are sorted by Kind then Name so celeris
// rows naturally cluster together.
func WriteMarkdown(w io.Writer, agg map[string]CellAggregate, meta Meta) error {
	// Preamble.
	ref := meta.GitRef
	if ref == "" {
		ref = "(unknown)"
	}
	if _, err := fmt.Fprintf(w, "# Celeris perfmatrix report — %s\n\n", ref); err != nil {
		return err
	}

	generated := meta.FinishedAt
	if generated.IsZero() {
		generated = time.Now().UTC()
	}
	if _, err := fmt.Fprintf(w, "Generated at %s", generated.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if meta.Host != "" {
		if _, err := fmt.Fprintf(w, " on %s", meta.Host); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}

	if meta.Runs > 0 || meta.Duration > 0 {
		if _, err := fmt.Fprintf(w, "Runs: %d × %s · Cells: %d\n",
			meta.Runs, meta.Duration, meta.TotalCells); err != nil {
			return err
		}
	}
	if meta.CelerisVer != "" || meta.LoadgenVer != "" {
		if _, err := fmt.Fprintf(w, "celeris=%s loadgen=%s\n",
			meta.CelerisVer, meta.LoadgenVer); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\n## Summary\n"); err != nil {
		return err
	}
	wins, neutral, regress := summaryCounts(agg)
	total := wins + neutral + regress
	if _, err := fmt.Fprintf(w, "- Celeris wins: %d/%d\n", wins, total); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "- Within noise (±2%%): %d\n", neutral); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "- Regressions vs best competitor: %d\n", regress); err != nil {
		return err
	}

	// Group aggregates by category, then by scenario within category.
	grouped := groupByCategory(agg)

	sections := []struct {
		title    string
		category string
	}{
		{"HTTP/1 — static scenarios", "static"},
		{"Driver scenarios", "driver"},
		{"Middleware chains", "chain"},
		{"Concurrency profiles", "concurrency"},
	}
	for _, sec := range sections {
		cells, ok := grouped[sec.category]
		if !ok || len(cells) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "\n## %s\n\n", sec.title); err != nil {
			return err
		}
		if err := writeSectionTable(w, cells, "rps"); err != nil {
			return err
		}
	}

	// Tail latency section spans every category.
	if len(agg) > 0 {
		if _, err := io.WriteString(w, "\n## Tail latency (P99, P99.9)\n\n"); err != nil {
			return err
		}
		// Flatten every aggregate; same scenario×server table layout.
		all := make([]CellAggregate, 0, len(agg))
		for _, v := range agg {
			all = append(all, v)
		}
		if err := writeSectionTable(w, all, "tail"); err != nil {
			return err
		}
	}

	return nil
}

// writeSectionTable renders a scenario-by-server table. `mode` selects
// which columns are written: "rps" (median RPS) or "tail" (P99/P99.9).
func writeSectionTable(w io.Writer, cells []CellAggregate, mode string) error {
	// Scenarios (rows) and servers (cols). Servers sorted by Kind then
	// Name so celeris rows cluster together.
	scenarioSet := map[string]struct{}{}
	serverSet := map[string]CellAggregate{} // keep one exemplar for Kind lookup
	for _, c := range cells {
		scenarioSet[c.ScenarioName] = struct{}{}
		if _, ok := serverSet[c.ServerName]; !ok {
			serverSet[c.ServerName] = c
		}
	}

	scenarios := make([]string, 0, len(scenarioSet))
	for s := range scenarioSet {
		scenarios = append(scenarios, s)
	}
	sort.Strings(scenarios)

	servers := make([]string, 0, len(serverSet))
	for s := range serverSet {
		servers = append(servers, s)
	}
	sort.Slice(servers, func(i, j int) bool {
		ki := serverSet[servers[i]].ServerKind
		kj := serverSet[servers[j]].ServerKind
		if ki == kj {
			return servers[i] < servers[j]
		}
		// celeris first so its rows cluster at the left.
		if ki == "celeris" {
			return true
		}
		if kj == "celeris" {
			return false
		}
		return ki < kj
	})

	// Build lookup so per-(scenario,server) cells are O(1).
	lookup := make(map[string]CellAggregate, len(cells))
	for _, c := range cells {
		lookup[CellID(c.ScenarioName, c.ServerName)] = c
	}

	// Header row.
	cols := append([]string{"scenario"}, servers...)
	if _, err := io.WriteString(w, "| "+strings.Join(cols, " | ")+" |\n"); err != nil {
		return err
	}
	sep := make([]string, len(cols))
	for i := range sep {
		sep[i] = "---"
	}
	if _, err := io.WriteString(w, "| "+strings.Join(sep, " | ")+" |\n"); err != nil {
		return err
	}

	for _, sc := range scenarios {
		// Determine the winner for this row (highest RPS / lowest P99).
		var best string
		switch mode {
		case "tail":
			bestDur := time.Duration(1<<62 - 1)
			for _, sv := range servers {
				c, ok := lookup[CellID(sc, sv)]
				if !ok || c.N == 0 {
					continue
				}
				if c.LatencyMedian.P99 > 0 && c.LatencyMedian.P99 < bestDur {
					bestDur = c.LatencyMedian.P99
					best = sv
				}
			}
		default:
			var bestRPS float64
			for _, sv := range servers {
				c, ok := lookup[CellID(sc, sv)]
				if !ok {
					continue
				}
				if c.RPSMedian > bestRPS {
					bestRPS = c.RPSMedian
					best = sv
				}
			}
		}

		row := []string{sc}
		for _, sv := range servers {
			c, ok := lookup[CellID(sc, sv)]
			if !ok || c.N == 0 {
				row = append(row, "—")
				continue
			}
			var cell string
			switch mode {
			case "tail":
				cell = fmt.Sprintf("%s / %s",
					formatDuration(c.LatencyMedian.P99),
					formatDuration(c.LatencyMedian.P9999))
			default:
				cell = fmt.Sprintf("%s rps", formatRPS(c.RPSMedian))
			}
			if sv == best {
				cell = "**" + cell + "**"
			}
			row = append(row, cell)
		}
		if _, err := io.WriteString(w, "| "+strings.Join(row, " | ")+" |\n"); err != nil {
			return err
		}
	}
	return nil
}

// summaryCounts groups cells into win / noise / regression vs the best
// non-celeris competitor in the same scenario row. A scenario counts as
// a win if any celeris server is >=2% faster than every non-celeris
// server; within noise if all celeris/competitor deltas lie within ±2%;
// regression if every celeris server is >=2% slower than the best
// competitor.
func summaryCounts(agg map[string]CellAggregate) (wins, neutral, regress int) {
	byScenario := map[string][]CellAggregate{}
	for _, c := range agg {
		byScenario[c.ScenarioName] = append(byScenario[c.ScenarioName], c)
	}
	for _, cells := range byScenario {
		var celerisBest, competitorBest float64
		for _, c := range cells {
			if c.ServerKind == "celeris" {
				if c.RPSMedian > celerisBest {
					celerisBest = c.RPSMedian
				}
			} else if c.RPSMedian > competitorBest {
				competitorBest = c.RPSMedian
			}
		}
		if celerisBest == 0 || competitorBest == 0 {
			continue
		}
		delta := (celerisBest - competitorBest) / competitorBest * 100
		switch {
		case delta > 2.0:
			wins++
		case delta < -2.0:
			regress++
		default:
			neutral++
		}
	}
	return wins, neutral, regress
}

// groupByCategory buckets aggregates into their Scenario category.
func groupByCategory(agg map[string]CellAggregate) map[string][]CellAggregate {
	out := make(map[string][]CellAggregate)
	for _, c := range agg {
		cat := c.Category
		if cat == "" {
			cat = "other"
		}
		out[cat] = append(out[cat], c)
	}
	return out
}

// formatRPS renders RPS with one decimal point and k/M suffixes so
// markdown tables stay compact.
func formatRPS(v float64) string {
	switch {
	case v >= 1_000_000:
		return fmt.Sprintf("%.2fM", v/1_000_000)
	case v >= 1_000:
		return fmt.Sprintf("%.1fk", v/1_000)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

// formatDuration prints a duration in human-friendly units (µs / ms).
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	if d >= time.Millisecond {
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	}
	return fmt.Sprintf("%dµs", d.Microseconds())
}
