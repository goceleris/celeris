//go:build memcached && !mcrtbugs

package memcached_test

// mcKnownBugs is false in the default build: cells that fail today are not run
// (and not skipped, a skip is not coverage). See roundtrip_bugs_on_test.go.
const mcKnownBugs = false
