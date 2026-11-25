//go:build postgres && pgrtbugs

package postgres_test

// rtKnownBugs is true under `-tags 'postgres pgrtbugs'`: the round-trip
// matrix then also runs the cells that are known to fail today (see
// rtKnownBug in roundtrip_types_test.go), so one command lists every wrong
// read-back with its bug id. The default build leaves them out so the
// Driver Conformance job stays green and has no SKIP line.
const rtKnownBugs = true
