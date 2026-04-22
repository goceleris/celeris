package report

// CSV output is wired through [WriteCSV] in aggregate.go. Wave-3 extracts
// the column-writer helpers into this file; it is kept separate so the
// top-level aggregator file stays small.
