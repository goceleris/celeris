package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// WriteCSV emits an aggregated.csv report. The file is tab-separated so
// Go's Printf float representation never collides with the field
// separator; spreadsheet tools ("import → tab-delimited") still open it
// transparently. Column order is stable across releases so the file
// diffs cleanly.
func WriteCSV(w io.Writer, agg map[string]CellAggregate) error {
	header := []string{
		"scenario",
		"server",
		"n",
		"rps_median",
		"rps_p5",
		"rps_p95",
		"rps_stddev",
		"latency_p50_us",
		"latency_p99_us",
		"latency_p9999_us",
		"errors",
		"bytes_median",
	}
	if _, err := io.WriteString(w, strings.Join(header, "\t")+"\n"); err != nil {
		return err
	}

	// Stable ordering: sort by cell id ("scenario/server") so repeated
	// runs produce byte-identical CSVs for trivial diffing.
	ids := make([]string, 0, len(agg))
	for id := range agg {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		c := agg[id]
		line := fmt.Sprintf(
			"%s\t%s\t%d\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%.2f\t%d\t%.2f\n",
			c.ScenarioName,
			c.ServerName,
			c.N,
			c.RPSMedian,
			c.RPSP5,
			c.RPSP95,
			c.RPSStdDev,
			float64(c.LatencyMedian.P50.Microseconds()),
			float64(c.LatencyMedian.P99.Microseconds()),
			float64(c.LatencyMedian.P9999.Microseconds()),
			c.Errors,
			c.BytesMedian,
		)
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
	}
	return nil
}
